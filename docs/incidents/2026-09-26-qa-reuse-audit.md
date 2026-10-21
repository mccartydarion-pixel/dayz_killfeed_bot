# Existing QA reuse audit (reliability staging, PR #108 at `05e02c9`)

**Nothing was created, deployed, migrated or changed.** No secret was read or printed. Existing QA services, production and PR #108's merge state were not touched.

## Access in this session

| Source | Status |
|---|---|
| GitHub | **available**: `refs/heads/staging/reliability-rc` = `05e02c913eb2d29383121c238398a4e3f197c7b8` (verified with `git ls-remote`) |
| Railway (`backboard.railway.app`) | **denied** by the network policy (proxy 403); no token, CLI or connector |
| Discord (`discord.com`) | **denied** by the network policy; no bot token |
| Repository records of the C.A.S.E. QA bot, guild or channel | **none**: no document in the repo names them |

So items 1, 3, 4 and 5 cannot be *observed* here; they are listed under "to verify" with exact read-only checks. Items 2 and 6 are answered from the code at `05e02c9`, with file references.

## Findings from the code

### F1. The C.A.S.E. QA bot token cannot safely run a second bot process at the same time

Two processes on one bot token each hold a gateway session and receive every event for every guild the bot is in.

1. **Every interaction is handled by both processes, with no guild filter.** The handlers in `internal/app/app.go` (around line 1118 onward) dispatch on command name only, and the command handlers only check that `i.GuildID` is non-empty. So a `/setup`, `/link` or `/admin` issued in either guild can be answered by either process, and handled against **that process's database**. The C.A.S.E. QA process could write the reliability guild's setup into the C.A.S.E. QA database, and the reliability process could do the reverse.
2. **Member-join events are handled twice** (welcome, verified-role work): `AddMemberJoinHandler` in `internal/discord/client.go`.
3. **They share one rate-limit budget.** Discord's global and per-route limits are per bot token. The reliability burst tests (40 cards and more) would delay or rate-limit C.A.S.E. QA traffic, and C.A.S.E. traffic would distort the reliability latency measurements.
4. **Bot presence flaps:** both processes set the bot's status (`UpdateStatusComplex`, `client.go`).

**Verdict:** concurrent reuse is **demonstrably unsafe** for C.A.S.E. QA. It is safe only if the C.A.S.E. QA bot service is **stopped for the whole reliability window** (see option A below). Stopping it is a change to an existing QA service and needs explicit approval.

### F2. The existing C.A.S.E. QA guild cannot host the reliability bot's `/setup`

* Discord `/setup` runs the V2 layout (`setup_commands.go` → `SetLayout(a.DiscordSetupLayout)`).
* The layout **reuses a category with the same name** (`resolveLayoutCategory`) and **adopts channels with matching names** inside it (`resolveLayoutChannel` / `matches`, `internal/app/saas_channel_layout.go`). The online counter matches *any* counter-format name.
* In a guild where the C.A.S.E. QA bot already ran `/setup`, the reliability bot would adopt C.A.S.E.'s `🔫・combat-feed`, counter and panel channels. It would post into them, delete its own cards there and rename the counter.

**Verdict:** reuse the C.A.S.E. QA **application** only together with a **separate test guild**. Do not reuse the C.A.S.E. QA guild or its synthetic delivery channel: posting reliability cards there contaminates C.A.S.E. QA evidence.

### F3. `/setup` cannot produce separate killfeed and deathfeed channels

* The V2 layout has one combat destination, `🔫・combat-feed`, routing `KILLFEED` and `PVE_FEED`.
* The death feed resolves the `KILLFEED` route (`app.go`: `deathFeed.SetRouteChannelResolver(publisher.RouteChannelID)`).
* The layout clears the legacy `DeathChannelID` and maps it to `KILLFEED` (`legacySetupReplacements`, `saas_api_channel_layout.go`).

So after `/setup`, kills and deaths share one channel **by product design**, each with its own 10-card window.

Requirement 6 (separate routes via `/setup`) **cannot be met at `05e02c9`**. The options:

* **(a) Recommended.** Test independence in the shipped shared-channel configuration. Each feed's window and journal are separate, and evictions never cross feeds. This is harness-tested by `TestDeathFeedIndependentInSharedChannel`; live, it is checked from channel state.
* **(b)** Separate channels through the legacy configuration: set `guild_setups.killfeed_channel_id` / `death_channel_id` directly in the **staging** database, with no `KILLFEED` route and without running `/setup`. This is not what V2 customers run.
* **(c)** A product change adding a separate death route: out of scope, and it needs a decision.

## Reuse matrix

| # | Resource | Exists? | Reusable for reliability testing? | Still needed |
|---|---|---|---|---|
| 1 | C.A.S.E. QA Discord **application / bot token** | per the owner, yes; **not verifiable here** | **Only by time-sharing** (F1): C.A.S.E. QA bot service stopped for the whole window, and the bot used in a separate test guild (F2) | approval to stop and restart the C.A.S.E. QA bot service; afterwards remove the bot from the reliability guild (or keep reliability stopped) so the restarted C.A.S.E. process never serves that guild. **Or option B:** a new application (permitted, because concurrent reuse is demonstrably unsafe) |
| 2 | C.A.S.E. QA **guild** | per the owner, yes; not verifiable | **No** (F2: `/setup` adopts C.A.S.E. channels by name) | a separate test guild (free; not a new application) |
| 3 | C.A.S.E. QA **synthetic delivery channel** | per the owner, yes; not verifiable | **No**: reliability cards would contaminate C.A.S.E. evidence, and the reliability window/journal would manage cards next to C.A.S.E. content | channels created by `/setup` in the reliability test guild |
| 4 | GitHub branch `staging/reliability-rc` | **yes, verified** = `05e02c9` | **yes**: pin R1 to it | nothing; never push to it |
| 5 | Railway `champions-case-staging` project | per the owner, yes; not verifiable | yes, as the host project | read-only inspection (section "To verify") |
| 6 | Billing QA service | per the owner, yes | **no**: not used, referenced or modified | — |
| 7 | "Three empty reliability QA service entries" | per the owner, yes; **not verifiable here** | **yes**, if they are empty and not linked to billing QA (checks below) | configure as `reliability-qa-bot` (repo, branch `staging/reliability-rc`), `reliability-qa-postgres` (Postgres, own volume) and `reliability-qa-nitrado-fixture` (`Dockerfile.nitrado-fixture`), with the variables in `2026-09-26-phase3-staging-deployment.md` section 3 |
| 8 | Reliability PostgreSQL | expected to be entry 7b | **yes**, if it is its own Postgres service with its own volume | `DATABASE_URL=${{<that service>.DATABASE_URL}}` only; confirm it is empty |
| 9 | Synthetic Nitrado source | code exists at `05e02c9` (`cmd/nitrado-fixture`) | yes | entry 7c; `FIXTURE_CONTROL_TOKEN` |
| 10 | Bot permissions (channel, role, Server Members) | unknown | depends on row 1 | checks below |
| 11 | Staging-only secrets | none exist for reliability | — | new `CREDENTIAL_ENCRYPTION_KEY`, `WEBSITE_API_SECRET`, `FIXTURE_CONTROL_TOKEN`. If reusing the C.A.S.E. token (option A), set it on R1 as a *copy*; never reference the C.A.S.E. service's variable |

## Recommended path

| Option | Discord | Effect on C.A.S.E. QA | Needs |
|---|---|---|---|
| **A: reuse the C.A.S.E. QA app (time-share)** | existing application, **new test guild** | C.A.S.E. QA bot service **stopped** during the reliability window; its commands are re-registered on restart | approval to stop/start the existing C.A.S.E. QA service; the owner invites the bot to the new guild; removal from that guild afterwards |
| **B: new QA application (recommended)** | new application in a new test guild | none: the C.A.S.E. service runs untouched and rate limits are independent | the owner creates the application (free) |

B is recommended because it needs no change to an existing QA service and gives clean latency measurements. A satisfies "no new application" at the cost of a C.A.S.E. QA outage during testing.

## To verify once access exists (read-only, names only, no secret values)

**Railway** (needs `RAILWAY_TOKEN` for `champions-case-staging`; network allowlist `backboard.railway.app`):

```sh
railway service list                                        # expect billing QA, C.A.S.E. QA (if hosted here), 3 reliability entries
railway variables --service <each reliability entry> --kv | cut -d= -f1 | sort   # expect empty
railway variables --kv | cut -d= -f1 | sort                 # shared variables: none may be referenced by reliability services
```

**Stop** if any reliability entry:

* has a source or deploy already set;
* references another service's variables (for example `${{<billing-qa>...}}`);
* shares a volume or database with billing QA or C.A.S.E. QA.

**Database isolation.** `DATABASE_URL` on the reliability bot is a reference to the reliability Postgres entry only. Its masked host is that service's private hostname. Before the first deploy, `SELECT to_regclass('schema_migrations')` returns NULL.

**Discord** (with the token the reliability bot will use; network allowlist `discord.com`):

```sh
GET /users/@me                         # bot identity (ID only)
GET /users/@me/guilds                  # option A: C.A.S.E. guild + reliability guild; option B: reliability guild only. Never a production guild.
GET /applications/@me                  # flags: GATEWAY_GUILD_MEMBERS (1<<14) or _LIMITED (1<<15) set = Server Members intent enabled
GET /guilds/{reliability}/members/{bot_id}   # the bot's roles
GET /guilds/{reliability}/roles        # permission bits of those roles
```

Required in the reliability guild:

* View Channel, Send Messages, Embed Links, Read Message History, Manage Messages, Manage Channels, Manage Roles, Connect;
* `applications.commands`;
* the bot's highest role above the Verified role;
* the Server Members intent enabled;
* **not** Administrator.

## Missing configuration and permissions (known now)

1. Network access to Railway and Discord, and a `champions-case-staging` project token, in this environment's settings.
2. The Discord decision: option A (with approval to stop the C.A.S.E. QA bot service) or option B.
3. A reliability test guild (either option).
4. A decision on requirement 6: (a) shared combat feed, recommended; (b) a legacy staging-DB configuration; or (c) a product change.
5. Configuration of the three reliability entries (phase 3 report, section 3) and new staging-only secrets.
6. The staging-only start command for the live SIGKILL test (phase 3 report, section 3).

Deployment, migrations and live tests remain pending explicit approval. Production is unchanged.
