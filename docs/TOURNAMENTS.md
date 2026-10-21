# Tournaments: single-elimination 1v1 and 2v2, scored from the kill feed

A tournament is run on one installation's DayZ server. The owner creates it (website or
`/tournament create`), players sign up and check in from Discord or the website, the bracket is
drawn at the start, and every kill the kill feed persists is matched against the match in play:
a kill between the two sides is a round, a kill with a weapon that is not allowed or outside the
arena is flagged for an admin, a kill by someone outside the match is interference. The winner
of the final takes the prizes (Champion Points, paid once) and the title, and the champion card
is posted.

Code: `internal/tournament` (the engine: the bracket, the match flow, the kill attribution and
the clock, all pure functions over one in-memory tournament, plus the `Service` that runs them
under a row lock), `internal/repository/tournament_repository.go` (the aggregate load and save),
`internal/discord/tournament_commands.go` and `tournament_announcer.go` (the `/tournament`
command, the sign-up card's buttons and every message), `internal/tournamentcard` (the bracket
and champion images, drawn with the Champion Card's kit `playercard.Surface`),
`internal/app/tournaments.go` (the wiring: kill pipeline, economy, scheduler, Discord) and
`internal/app/saas_api_tournaments.go` (the HTTP routes). Migration `0132_tournaments`.

## 1. Life cycle

| Status | Meaning |
| --- | --- |
| `DRAFT` | Created, not visible to players. The owner edits it. |
| `SIGNUP` | Players join (and leave). Opened by the owner (`open`) or by the clock at `signupOpensAt`. |
| `CHECKIN` | Opened by the clock at `startsAt − checkinMinutes` (when `checkinMinutes > 0`). Entries confirm they are present; an entry that does not check in is withdrawn at the start. Joining during check-in checks the entry in at once. |
| `LIVE` | The bracket is drawn and kills count. Entered by the owner (`start`, which counts every entry as present) or by the clock at `startsAt`; with fewer than two checked-in entries the clock cancels instead. |
| `PAUSED` | Kills do not count; timers are cleared and set again on `resume`. |
| `FINISHED` | The final has a result. Entries are `WINNER` / `ELIMINATED`, prizes are paid, the title is written, the champion is announced. |
| `CANCELLED` | Ended without a result. |

Bracket: single elimination for 4, 8, 16 or 32 slots. Seeds fill the standard order (1 v 16,
8 v 9, 4 v 13, ...; `tournament.SeedOrder`) so the top seeds cannot meet before the final.
`RANKED` seeding orders entries by their Ranked Points in the server's active season (a team's
players added; ties by check-in time), `RANDOM` shuffles them with a seeded draw. Slots without
an entry are byes: a match with one side is over before it starts (`DONE`, that side the winner)
and the walkover cascades through later rounds. Round names: Round of 16, Quarter-final,
Semi-final, Final; for 32 slots the first round is "Round 1". Every match links to the match its
winner goes to (`nextMatchId`).

Match flow (`tournament_matches.status`):

* `PENDING` → `CALLED` when the match is called: the next arena in rotation over the configured
  arenas is assigned, `timer_ends_at = now + readyMinutes`, the players are mentioned.
* `CALLED` → `LIVE` on the first counted round, or when an admin starts it (`StartMatch`), or when
  the ready timer runs out with both sides on the server. `timer_ends_at = now + matchTimerMinutes`.
* Rounds until one side has `ceil(bestOf / 2)` → `DONE`; the winner advances and the next match
  is called at once. In a 2v2 a round is a kill: the match is first to `ceil(bestOf / 2)` kills by
  either member of a team.
* Ready timer over: one side absent (not every player of the entry connected) → the other wins
  by `FORFEIT`; both absent → an admin is pinged and the match waits with no timer (`call` again,
  `result` or `dq`).
* Match timer over: an admin is pinged; nothing is decided by itself.
* The tournament finishes when the final is `DONE` or `FORFEIT`.

Places: 1 the champion, 2 the finalist, 3 both semi-final losers, 5 the quarter-final losers, 9,
17 (the usual tied placings). A prize for a place is paid to every player of every entry that
finished there.

## 2. Kill attribution

The persistence adapter's `ProcessPersistedKill` (the same hook that awards Ranked Points, claims
bounties and closes lives: `internal/app/persistence_store_adapter.go`) calls
`Service.OnKill` for every durably persisted, non-duplicate kill; `ProcessPersistedDeath` calls
`OnDeath` for a non-player death. Only a server with a `LIVE` tournament does anything.
Everything below happens in one transaction with the tournament row locked
(`TournamentRepository.Transact`); a kill is attributed at most once (`tournament_rounds.kill_id`
is unique, and a loaded tournament already carrying the kill is a no-op).

| Situation | Round |
| --- | --- |
| Killer and victim are on the two sides of a `CALLED` or `LIVE` match | `counted = true`, the killer's side wins the round. The match starts if it was only called. |
| ... but `allowedWeapons` is set and the weapon is not in it (`NormalizeWeapon`: case, spaces, hyphens, underscores and dots do not matter, so `M4-A1` = `m4a1`) | `flag = WEAPON`, not counted, admin ping. |
| ... but the match has an arena and the killer's logged position is farther than its radius from the centre (a kill with no position counts) | `flag = OUTSIDE_ARENA`, not counted, admin ping. |
| Only one of them is in a live match (or they are team-mates) | `flag = INTERFERENCE` on that match, no winner, not counted, admin ping. |
| A player of a live match dies to something other than a player (`OnDeath`) | `flag = NON_PLAYER`, not counted, admin ping saying to replay or decide. |
| Neither is in a live match | Nothing. |

Admin decisions write `MANUAL` rounds with `decided_by`: `result` (a counted round for the winner
that settles the match outright), `replay` (every earlier round of the match stops counting, the
scores clear and the match is called again; a decided match can be replayed only while the next
match has not begun, and its winner's slot there is cleared), `dq` (the entry is `DQ`; its current
match is forfeited to the opponent, or its later slot is cleared so the opponent walks over).

## 3. Discord

`/tournament`, registered on the guild like the other commands. Admin subcommands need
Administrator or Manage Server (`isAdminInteraction`, as for `/economy credit` and the other
admin commands); the tournament's server is the guild's selected public server and it must back
a Champion installation.

| Subcommand | Who | What |
| --- | --- | --- |
| `create name start [team_size] [bracket_size] [best_of] [seeding] [weapons] [prizes] [title] [checkin_minutes] [arena]` | admin | A `DRAFT` in this channel. `start` is a UTC time (`2026-10-10 19:00`), a clock time today/tomorrow (`19:00`) or a delay (`90m`, `2h`). `weapons` and `prizes` are comma-separated (`M4-A1,KA-M`; `1000,500,250` for 1st, 2nd, 3rd), `title` is the winner's title, `arena` is `x,z,radius`. Without `arena`, the built stadium (docs/STADIUM.md) becomes "Stadium": its centre, with a radius that covers its footprint. With no stadium there is no arena and kills anywhere count. |
| `open` | admin | Opens sign-up (the newest draft when nothing is open) and posts the sign-up card. |
| `start`, `pause`, `resume`, `cancel`, `call` | admin | As in section 1; `call` re-calls the match in play (its ready timer starts again) or calls the next one. |
| `result match winner [note]`, `replay match`, `dq player [reason]` | admin | `match` is the match number `/tournament status` lists (bracket order); `winner`/`player` are names as shown. |
| `join [partner]` | player | Needs a `VERIFIED` link (`/link`); a 2v2 entry needs a partner with one too. During check-in the entry is checked in at once. |
| `leave`, `checkin`, `status` | player | `status` says where the tournament stands, the caller's next match and every match with its score. |
| `bracket` | anyone | Posts the bracket image in the channel. |

Messages (all in the tournament's channel; the announcer is `tournament.Notifier`):

* The sign-up card: name, start, check-in, format, weapons, arenas, prizes and the entry list
  (✅ checked in, ⛔ disqualified) with **Join**, **Leave** and **Check in** buttons
  (`champion_tourney_<action>:<id>`; Join is disabled for 2v2, where the partner is named with
  `/tournament join`; Check in is enabled during check-in). Edited in place on every change;
  the buttons go at the start.
* The bracket image (`internal/tournamentcard`, 1200 px wide, taller for bigger brackets):
  every match with seeds, small tier emblems, names and scores, the match in play outlined in
  gold with its arena, called matches in amber, byes and forfeits labelled. Posted at the start
  and re-posted (the previous one deleted) after every match result, replay or walkover, and at
  the end.
* A match call with both sides mentioned, the arena and its radius, the weapons and the ready
  deadline; a line when the match goes live; a line per round (counted or why not); the match
  result; pause, resume and cancel notices.
* Admin pings go to the installation's `ADMIN_ALERTS` route when it has one, else the
  tournament's channel, mentioning the admin who created the tournament.
* The champion: the winner card (name, emblem, title, matches, rounds, prize) with the prizes
  paid per place.

## 4. HTTP API

### Public

`GET /api/saas/network/servers/{installationID}/tournament` (service bearer only, no acting
user; the website serves it at `/api/live/{installationID}/tournament`). Cached 3 s per
installation. `404 NOT_FOUND` when the installation has no server.

```json
{
  "installationId": 7,
  "server": { "name": "...", "platform": "PLAYSTATION", "map": "chernarusplus", "discordInvite": "https://discord.gg/...", "onlinePlayers": 38 },
  "tournament": { ... } | null,
  "fighters": { "<playerId>": { "kills", "deaths", "kd", "headshots", "longestKillMeters", "favouriteWeapon", "record": { "wins", "losses", "roundsWon", "roundsLost" } } },
  "generatedAt": "RFC3339"
}
```

`tournament` is the installation's most relevant one: `LIVE`/`PAUSED` first, then `CHECKIN`,
then `SIGNUP` (the one starting soonest), else the latest one `FINISHED` or `CANCELLED` within
the last 24 hours, else `null`. Its shape (`tournament.TournamentDTO`, pinned by
`TestLiveShapeMatchesFixture` against `internal/tournament/testdata/live-fixture.json`):

```json
{
  "id", "name", "status", "format": "SINGLE_ELIM", "teamSize", "bracketSize", "bestOf", "seeding",
  "startsAt", "signupOpensAt" | null, "checkinMinutes", "startedAt" | null, "finishedAt" | null,
  "rules": { "allowedWeapons": [], "arenas": [{ "no", "name", "x", "z", "radius" }], "matchTimerMinutes", "readyMinutes" },
  "prizes": [{ "place", "points", "title" | null }],
  "entries": [{ "id", "seed" | null, "teamNo", "checkedIn", "status", "players": [{ "playerId", "name", "rankTier", "faction": { "tag", "name", "color" } | null }] }],
  "matches": [{ "id", "round", "position", "roundName", "a" | null, "b" | null, "status", "scoreA", "scoreB", "winner" | null, "arena" | null,
               "calledAt" | null, "startedAt" | null, "endedAt" | null, "nextMatchId" | null,
               "rounds": [{ "n", "winner" | null, "killerName", "victimName", "weapon", "distance" | null, "at", "counted", "flag" | null }] }],
  "current": { "matchId", "timerEndsAt" | null } | null,
  "results": [ the ten latest rounds, newest first, each with "matchId" ],
  "champion": { "entryId", "players": [...], "title" | null } | null
}
```

`rankTier` is the player's tier in the server's active Ranked season (`UNRANKED` without one);
`faction` is the Faction Hub faction of the player's link holder on the installation. `fighters`
holds every entry player's figures on this server (kills, deaths including PvP deaths, K/D to two
decimals, headshots, longest kill, the weapon with most kills) and their tournament record
(byes do not count as matches).

### Owner

Base `/api/saas/organizations/{organizationID}/installations/{installationID}/tournaments`. The
standard chain: service auth, acting user, organization OWNER or ADMIN (`403 FORBIDDEN`), the
installation in the organization (`404 NOT_FOUND`); a view-as session may only `GET`. Reads use
the admin read budget, writes the admin action budget (`429 RATE_LIMITED`). Every write is
audited (`TOURNAMENT_CREATE`, `_UPDATE`, `_OPEN`, `_START`, `_PAUSE`, `_RESUME`, `_CANCEL`,
`_CALL`, `_RESULT`, `_REPLAY`, `_DQ`).

| Route | Purpose |
| --- | --- |
| `GET` | `{"items": [Tournament]}`: the newest 50. |
| `POST` | Create a `DRAFT`. Body: `{name, teamSize (1), bracketSize (8), bestOf (1), seeding ("RANDOM"), startsAt (RFC 3339, required), signupOpensAt?, checkinMinutes (30), rules{allowedWeapons[], arenas[], matchTimerMinutes (10), readyMinutes (3)}, prizes[{place, points, title?}], discordChannelId?}`. Without `rules.arenas` the built stadium becomes the arena (as in Discord). Without `discordChannelId` the installation's `EVENTS` route channel is used; with neither, nothing is posted to Discord and the tournament still runs. `201` with the tournament. |
| `GET /{tid}` | The tournament. |
| `PATCH /{tid}` | Edits while `DRAFT` or `SIGNUP` (fields left out keep their value; `409` once entries exist and the team size would change or the bracket would shrink below them). |
| `POST /{tid}/open`, `/start`, `/pause`, `/resume`, `/cancel`, `/call` | The transitions of section 1 and 3. `409 CONFLICT` when the state does not allow it (the message says why, e.g. "at least two checked-in entries are needed"). |
| `POST /{tid}/matches/{mid}/result` `{winnerEntryId, note?}` | An admin decision. `409` unless the winner is in that unfinished match. |
| `POST /{tid}/matches/{mid}/replay` | Replay the match. |
| `POST /{tid}/entries/{eid}/dq` `{reason?}` | Disqualify an entry. |

Responses are `{"tournament": Tournament}` where `Tournament` is the public shape plus
`flags` (the rounds of unfinished matches that need a ruling: `{matchId, n, flag, killerName,
victimName, weapon, at, winner, decidedBy}`), `discordChannelId`, `createdAt` and `updatedAt`.
Validation errors are `400 VALIDATION_ERROR` with a plain message.

### Player

Base `/api/saas/player/servers/{installationID}/tournaments`: service auth and acting user, the
installation must exist with a server (`404`). Like the map vote (docs/PLAYER_API.md section 9)
any signed-in user may read; writing needs a `VERIFIED` link for the installation's guild
(`409 PLAYER_IDENTITY_REQUIRED`), not observed activity, so a player who has just linked can
enter.

| Route | Purpose |
| --- | --- |
| `GET` | `{installationId, linked, current: Tournament | null, me: {entryId, status, checkedIn, seed, nextMatch: {matchId, roundName, status, opponent, arena, scoreMe, scoreThem} | null, place} | null, past: [{id, name, status, startsAt, finishedAt, champion, myPlace}]}`. `current` is the tournament the player can act on (`SIGNUP`, `CHECKIN`, `LIVE` or `PAUSED`); `past` the latest ten that ended. |
| `POST /{tid}/join` `{partnerDiscordId?}` | Enter (a 2v2 needs a partner with a verified link: `400 VALIDATION_ERROR` otherwise). `409 CONFLICT` when already entered, the bracket is full or sign-up is not open. |
| `POST /{tid}/leave`, `POST /{tid}/checkin` | Withdraw; check in (`409` before check-in opens). |

Writes answer `{"tournament": Tournament, "me": ...}`.

The title: `tournament_titles` holds one title per player and installation (the latest win
replaces it). It is `tournamentTitle` on `GET .../player/servers/{installationID}/stats` and on
the Champion Card data (`GET .../card`), `null` when the player holds none.

## 5. Prizes

When a tournament finishes, `Service.SettlePrizes` runs after the commit: for every entry with
a place that has a prize, every player of the entry gets the points as an earned
`SYSTEM_REWARD` credit through the economy service (`Service.Credit`, reference
`tournament:<id>:entry:<entryId>`, description "Tournament prize: 1st place in <name>"). A
payout is recorded first in `tournament_payouts` (one row per tournament, entry and player;
`ON CONFLICT DO NOTHING`) and the credit's own reference is idempotent too, so a retry pays
nothing twice. The place-1 prize's `title` is written to `tournament_titles`.

## 6. Scheduler

`startTournamentScheduler` runs `Service.Tick` every 5 seconds on the leader
(`singleton_leader.go`). `TournamentRepository.Due` lists the tournaments with something due:
a draft whose `signup_opens_at` has passed, a `SIGNUP`/`CHECKIN` one whose check-in or start
time has passed, a `LIVE` one whose match in play has a timer at or before now. Each is handled
in its own transaction (`Tournament.Tick`). Presence for a ready timeout is
`player_server_activity.currently_connected` on the tournament's server.

## 7. Database (migration `0132_tournaments`)

`tournaments` (installation, guild, server, name, status, format, team_size, bracket_size,
best_of, seeding, starts_at, signup_opens_at, checkin_minutes, started_at, finished_at, rules
JSONB, prizes JSONB, discord_channel_id, signup_message_id, bracket_message_id,
created_by_discord_id, timestamps), `tournament_entries` (seed, team_no, checked_in_at, status),
`tournament_entry_players` (entry, player, discord_user_id, player_name snapshot),
`tournament_matches` (round, position, round_name, entry_a, entry_b, status, arena_no, score_a,
score_b, winner_entry, next_match_id, called_at, started_at, ended_at, timer_ends_at),
`tournament_rounds` (match, n, winner_entry, kill_id unique, killer_name, victim_name, weapon,
distance, at, counted, flag, decided_by), `tournament_titles` (installation, player, title,
tournament, awarded_at) and `tournament_payouts` (tournament, entry, player, place, points,
transaction_id, paid_at).

## 8. Tests

* `internal/tournament` (unit, `-race` clean): the seed order and round names for every size,
  full brackets with next links, byes (5 in 8; 2 in 32 cascading to the final), random seeding
  reproducible per seed, sign-up (join, double join, full, leave and come back, check-in),
  2v2 entries and team-mate kills, a best-of-three match with the first counted kill starting
  it, every flag (weapon, normalised weapon, outside and inside the arena, interference both
  ways, non-player death, nothing while paused), forfeits and both timers, admin results,
  pause/resume, replay (and its refusal once the next match began), DQ before and during play,
  the clock (open, check-in, start, cancel), parameter validation, the service over an in-memory
  store (life cycle through `OnKill`, duplicate and foreign-server kills ignored, prizes and the
  title paid once, rollback on a refused transition) and the fixture key-shape test.
* `internal/tournamentcard`: sizes per bracket, deterministic bytes, the champion card.
* `internal/discord`: the sign-up card and buttons, the announcer's flow with a fake sender
  (card edited in place, match call mentions, bracket re-posted, admin ping, champion card), the
  design rules on every embed, `/tournament status` text, start-time parsing.
* `internal/app/saas_api_tournaments_integration_test.go` (routes + PostgreSQL + the real
  persistence queue): a 1v1 life cycle (authorization, create, view-as read-only, patch, open,
  joins incl. unlinked and a full bracket, the clock opening check-in, check-ins, start, kills
  through the pipeline: interference, a counted round, a weapon flag, a replayed kill, an
  outside-arena flag, an admin result, finish, prizes paid once, the title on the stats and
  card routes, the public shape) and a 2v2 life cycle with partners, a team withdrawing and
  returning, byes to the final, both team members paid, plus a tournament cancelled by the
  clock for want of check-ins.
