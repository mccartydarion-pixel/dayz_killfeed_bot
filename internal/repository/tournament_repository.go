package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/tournament"
)

// Tournament mode (docs/TOURNAMENTS.md): the tournament aggregate (tournaments, entries with
// their players, matches with their rounds) loaded whole and written back whole inside one
// transaction under a row lock, plus the reads the API and the Discord side need around it.

type TournamentRepository struct{ pool *pgxpool.Pool }

func NewTournamentRepository(pool *pgxpool.Pool) *TournamentRepository {
	return &TournamentRepository{pool: pool}
}

// ErrKillAttributed is a round for a kill that already has one.
var ErrKillAttributed = errors.New("kill already attributed to a round")

const tournamentCols = `id, installation_id, guild_id, server_id, name, status, format, team_size, bracket_size, best_of, seeding, starts_at, signup_opens_at,
checkin_minutes, started_at, finished_at, rules, prizes, COALESCE(discord_channel_id,''), COALESCE(signup_message_id,''), COALESCE(bracket_message_id,''),
COALESCE(created_by_discord_id,''), created_at, updated_at`

// execer is the read surface shared by the pool and a transaction.
type execer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func scanTournament(row pgx.Row) (*tournament.Tournament, error) {
	var t tournament.Tournament
	var rules, prizes []byte
	err := row.Scan(&t.ID, &t.InstallationID, &t.GuildID, &t.ServerID, &t.Name, &t.Status, &t.Format, &t.TeamSize, &t.BracketSize, &t.BestOf, &t.Seeding, &t.StartsAt, &t.SignupOpensAt,
		&t.CheckinMinutes, &t.StartedAt, &t.FinishedAt, &rules, &prizes, &t.DiscordChannelID, &t.SignupMessageID, &t.BracketMessageID, &t.CreatedByDiscordID, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tournament.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(rules) > 0 {
		if err := json.Unmarshal(rules, &t.Rules); err != nil {
			return nil, fmt.Errorf("tournament %d rules: %w", t.ID, err)
		}
	}
	if len(prizes) > 0 {
		if err := json.Unmarshal(prizes, &t.Prizes); err != nil {
			return nil, fmt.Errorf("tournament %d prizes: %w", t.ID, err)
		}
	}
	if t.Prizes == nil {
		t.Prizes = []tournament.Prize{}
	}
	if t.Rules.Arenas == nil {
		t.Rules.Arenas = []tournament.Arena{}
	}
	if t.Rules.AllowedWeapons == nil {
		t.Rules.AllowedWeapons = []string{}
	}
	return &t, nil
}

// load reads the whole aggregate; lock adds FOR UPDATE on the tournament row.
func (r *TournamentRepository) load(ctx context.Context, q execer, id int64, lock bool) (*tournament.Tournament, error) {
	sql := `SELECT ` + tournamentCols + ` FROM tournaments WHERE id=$1`
	if lock {
		sql += ` FOR UPDATE`
	}
	t, err := scanTournament(q.QueryRow(ctx, sql, id))
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT e.id, e.seed, e.team_no, e.checked_in_at, e.status, e.created_at FROM tournament_entries e WHERE e.tournament_id=$1 ORDER BY e.team_no`, id)
	if err != nil {
		return nil, err
	}
	byEntry := map[int64]*tournament.Entry{}
	for rows.Next() {
		var e tournament.Entry
		if err := rows.Scan(&e.ID, &e.Seed, &e.TeamNo, &e.CheckedInAt, &e.Status, &e.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		e.Players = []tournament.EntryPlayer{}
		t.Entries = append(t.Entries, &e)
		byEntry[e.ID] = &e
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT p.entry_id, p.player_id, p.discord_user_id, p.player_name FROM tournament_entry_players p
JOIN tournament_entries e ON e.id=p.entry_id WHERE e.tournament_id=$1 ORDER BY p.entry_id, p.player_id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var entryID int64
		var p tournament.EntryPlayer
		if err := rows.Scan(&entryID, &p.PlayerID, &p.DiscordUserID, &p.Name); err != nil {
			rows.Close()
			return nil, err
		}
		if e := byEntry[entryID]; e != nil {
			e.Players = append(e.Players, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT id, round, position, round_name, entry_a, entry_b, status, arena_no, score_a, score_b, winner_entry, next_match_id, called_at, started_at, ended_at, timer_ends_at
FROM tournament_matches WHERE tournament_id=$1 ORDER BY round, position`, id)
	if err != nil {
		return nil, err
	}
	byMatch := map[int64]*tournament.Match{}
	for rows.Next() {
		var m tournament.Match
		if err := rows.Scan(&m.ID, &m.Round, &m.Position, &m.RoundName, &m.EntryA, &m.EntryB, &m.Status, &m.ArenaNo, &m.ScoreA, &m.ScoreB, &m.WinnerEntry, &m.NextMatchID, &m.CalledAt, &m.StartedAt, &m.EndedAt, &m.TimerEndsAt); err != nil {
			rows.Close()
			return nil, err
		}
		m.Rounds = []tournament.Round{}
		t.Matches = append(t.Matches, &m)
		byMatch[m.ID] = &m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT r.id, r.match_id, r.n, r.winner_entry, r.kill_id, r.killer_name, r.victim_name, r.weapon, r.distance, r.at, r.counted, r.flag, r.decided_by
FROM tournament_rounds r JOIN tournament_matches m ON m.id=r.match_id WHERE m.tournament_id=$1 ORDER BY r.match_id, r.n`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rd tournament.Round
		if err := rows.Scan(&rd.ID, &rd.MatchID, &rd.N, &rd.WinnerEntry, &rd.KillID, &rd.KillerName, &rd.VictimName, &rd.Weapon, &rd.Distance, &rd.At, &rd.Counted, &rd.Flag, &rd.DecidedBy); err != nil {
			rows.Close()
			return nil, err
		}
		if m := byMatch[rd.MatchID]; m != nil {
			m.Rounds = append(m.Rounds, rd)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	t.RestoreNextPos()
	return t, nil
}

// Get loads a tournament without locking it.
func (r *TournamentRepository) Get(ctx context.Context, id int64) (*tournament.Tournament, error) {
	return r.load(ctx, r.pool, id, false)
}

// Create inserts the tournament row (entries and matches are written by Transact).
func (r *TournamentRepository) Create(ctx context.Context, t *tournament.Tournament) error {
	rules, err := json.Marshal(t.Rules)
	if err != nil {
		return err
	}
	prizes, err := json.Marshal(t.Prizes)
	if err != nil {
		return err
	}
	return r.pool.QueryRow(ctx, `INSERT INTO tournaments (installation_id, guild_id, server_id, name, status, format, team_size, bracket_size, best_of, seeding, starts_at, signup_opens_at,
checkin_minutes, rules, prizes, discord_channel_id, created_by_discord_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,NULLIF($16,''),NULLIF($17,'')) RETURNING id, created_at, updated_at`,
		t.InstallationID, t.GuildID, t.ServerID, t.Name, t.Status, t.Format, t.TeamSize, t.BracketSize, t.BestOf, t.Seeding, t.StartsAt, t.SignupOpensAt,
		t.CheckinMinutes, rules, prizes, t.DiscordChannelID, t.CreatedByDiscordID).Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt)
}

// Transact locks the tournament, loads it, runs fn and writes back the aggregate.
func (r *TournamentRepository) Transact(ctx context.Context, id int64, fn func(t *tournament.Tournament) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	t, err := r.load(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if err := fn(t); err != nil {
		return err
	}
	if err := r.save(ctx, tx, t); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// save writes the aggregate: the row, every entry (new ones inserted), every match (new ones
// inserted, then linked) and every new round. A round for a kill that already has one is a
// unique violation, reported as ErrKillAttributed so the transaction rolls back.
func (r *TournamentRepository) save(ctx context.Context, tx pgx.Tx, t *tournament.Tournament) error {
	rules, err := json.Marshal(t.Rules)
	if err != nil {
		return err
	}
	prizes, err := json.Marshal(t.Prizes)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE tournaments SET name=$2, status=$3, team_size=$4, bracket_size=$5, best_of=$6, seeding=$7, starts_at=$8, signup_opens_at=$9, checkin_minutes=$10,
started_at=$11, finished_at=$12, rules=$13, prizes=$14, discord_channel_id=NULLIF($15,''), signup_message_id=NULLIF($16,''), bracket_message_id=NULLIF($17,''), updated_at=NOW() WHERE id=$1`,
		t.ID, t.Name, t.Status, t.TeamSize, t.BracketSize, t.BestOf, t.Seeding, t.StartsAt, t.SignupOpensAt, t.CheckinMinutes, t.StartedAt, t.FinishedAt, rules, prizes,
		t.DiscordChannelID, t.SignupMessageID, t.BracketMessageID); err != nil {
		return err
	}
	for _, e := range t.Entries {
		if e.ID == 0 {
			if err := tx.QueryRow(ctx, `INSERT INTO tournament_entries (tournament_id, seed, team_no, checked_in_at, status, created_at) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
				t.ID, e.Seed, e.TeamNo, e.CheckedInAt, e.Status, e.CreatedAt).Scan(&e.ID); err != nil {
				return err
			}
			for _, p := range e.Players {
				if _, err := tx.Exec(ctx, `INSERT INTO tournament_entry_players (entry_id, player_id, discord_user_id, player_name) VALUES ($1,$2,$3,$4)`, e.ID, p.PlayerID, p.DiscordUserID, p.Name); err != nil {
					return err
				}
			}
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE tournament_entries SET seed=$2, checked_in_at=$3, status=$4 WHERE id=$1 AND tournament_id=$5`, e.ID, e.Seed, e.CheckedInAt, e.Status, t.ID); err != nil {
			return err
		}
	}
	inserted := false
	for _, m := range t.Matches {
		if m.ID != 0 {
			continue
		}
		inserted = true
		if err := tx.QueryRow(ctx, `INSERT INTO tournament_matches (tournament_id, round, position, round_name, entry_a, entry_b, status, arena_no, score_a, score_b, winner_entry, called_at, started_at, ended_at, timer_ends_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id`,
			t.ID, m.Round, m.Position, m.RoundName, m.EntryA, m.EntryB, m.Status, m.ArenaNo, m.ScoreA, m.ScoreB, m.WinnerEntry, m.CalledAt, m.StartedAt, m.EndedAt, m.TimerEndsAt).Scan(&m.ID); err != nil {
			return err
		}
	}
	if inserted {
		t.LinkNext()
	}
	for _, m := range t.Matches {
		if _, err := tx.Exec(ctx, `UPDATE tournament_matches SET entry_a=$2, entry_b=$3, status=$4, arena_no=$5, score_a=$6, score_b=$7, winner_entry=$8, next_match_id=$9, called_at=$10, started_at=$11, ended_at=$12, timer_ends_at=$13
WHERE id=$1 AND tournament_id=$14`, m.ID, m.EntryA, m.EntryB, m.Status, m.ArenaNo, m.ScoreA, m.ScoreB, m.WinnerEntry, m.NextMatchID, m.CalledAt, m.StartedAt, m.EndedAt, m.TimerEndsAt, t.ID); err != nil {
			return err
		}
		for i := range m.Rounds {
			rd := &m.Rounds[i]
			rd.MatchID = m.ID
			if rd.ID != 0 {
				if _, err := tx.Exec(ctx, `UPDATE tournament_rounds SET counted=$2, winner_entry=$3 WHERE id=$1`, rd.ID, rd.Counted, rd.WinnerEntry); err != nil {
					return err
				}
				continue
			}
			err := tx.QueryRow(ctx, `INSERT INTO tournament_rounds (match_id, n, winner_entry, kill_id, killer_name, victim_name, weapon, distance, at, counted, flag, decided_by)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`, m.ID, rd.N, rd.WinnerEntry, rd.KillID, rd.KillerName, rd.VictimName, rd.Weapon, rd.Distance, rd.At, rd.Counted, rd.Flag, rd.DecidedBy).Scan(&rd.ID)
			if isUniqueViolation(err) {
				return ErrKillAttributed
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// LiveForServer is the LIVE tournament of a server (0 when none).
func (r *TournamentRepository) LiveForServer(ctx context.Context, serverID int64) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT id FROM tournaments WHERE server_id=$1 AND status='LIVE' ORDER BY started_at DESC NULLS LAST, id DESC LIMIT 1`, serverID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// Due lists the tournaments the clock may act on at now.
func (r *TournamentRepository) Due(ctx context.Context, now time.Time) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
SELECT t.id FROM tournaments t
WHERE (t.status='DRAFT' AND t.signup_opens_at IS NOT NULL AND t.signup_opens_at <= $1)
   OR (t.status IN ('SIGNUP','CHECKIN') AND (t.starts_at <= $1 OR t.starts_at - make_interval(mins => t.checkin_minutes) <= $1))
   OR (t.status='LIVE' AND EXISTS (SELECT 1 FROM tournament_matches m WHERE m.tournament_id=t.id AND m.status IN ('CALLED','LIVE') AND m.timer_ends_at IS NOT NULL AND m.timer_ends_at <= $1))
ORDER BY t.id`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RecordPayout records a prize payment once per tournament, entry and player.
func (r *TournamentRepository) RecordPayout(ctx context.Context, p tournament.Payout) (bool, error) {
	tag, err := r.pool.Exec(ctx, `INSERT INTO tournament_payouts (tournament_id, entry_id, player_id, place, points, transaction_id) VALUES ($1,$2,$3,$4,$5,NULLIF($6,0))
ON CONFLICT (tournament_id, entry_id, player_id) DO NOTHING`, p.TournamentID, p.EntryID, p.PlayerID, p.Place, p.Points, p.TransactionID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetTitle gives a player the installation's tournament title (the latest win replaces an
// earlier one).
func (r *TournamentRepository) SetTitle(ctx context.Context, installationID, playerID int64, title string, tournamentID int64) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO tournament_titles (installation_id, player_id, title, tournament_id, awarded_at) VALUES ($1,$2,$3,$4,NOW())
ON CONFLICT (installation_id, player_id) DO UPDATE SET title=EXCLUDED.title, tournament_id=EXCLUDED.tournament_id, awarded_at=NOW()`, installationID, playerID, title, tournamentID)
	return err
}

// Title is a player's tournament title on an installation ("" when none).
func (r *TournamentRepository) Title(ctx context.Context, installationID, playerID int64) (string, error) {
	var title string
	err := r.pool.QueryRow(ctx, `SELECT title FROM tournament_titles WHERE installation_id=$1 AND player_id=$2`, installationID, playerID).Scan(&title)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return title, err
}

// ListForInstallation lists an installation's tournaments, newest first, up to limit (ids and
// the rows only; entries and matches are loaded per tournament).
func (r *TournamentRepository) ListForInstallation(ctx context.Context, installationID int64, limit int) ([]*tournament.Tournament, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+tournamentCols+` FROM tournaments WHERE installation_id=$1 ORDER BY starts_at DESC, id DESC LIMIT $2`, installationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tournament.Tournament
	for rows.Next() {
		t, err := scanTournament(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Relevant is the installation's tournament the live page shows: one in SIGNUP, CHECKIN, LIVE
// or PAUSED (the one starting soonest), else one that finished or was cancelled within the last
// day (the latest). 0 when there is none.
func (r *TournamentRepository) Relevant(ctx context.Context, installationID int64, now time.Time) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `
SELECT id FROM tournaments WHERE installation_id=$1 AND (
    status IN ('SIGNUP','CHECKIN','LIVE','PAUSED') OR (status IN ('FINISHED','CANCELLED') AND finished_at >= $2))
ORDER BY CASE status WHEN 'LIVE' THEN 0 WHEN 'PAUSED' THEN 0 WHEN 'CHECKIN' THEN 1 WHEN 'SIGNUP' THEN 2 ELSE 3 END,
         CASE WHEN status IN ('SIGNUP','CHECKIN','LIVE','PAUSED') THEN starts_at END ASC, finished_at DESC, id DESC LIMIT 1`,
		installationID, now.Add(-24*time.Hour)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// Current is the installation's tournament a player can act on (SIGNUP, CHECKIN, LIVE or PAUSED,
// the soonest), 0 when none.
func (r *TournamentRepository) Current(ctx context.Context, installationID int64) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT id FROM tournaments WHERE installation_id=$1 AND status IN ('SIGNUP','CHECKIN','LIVE','PAUSED')
ORDER BY CASE status WHEN 'LIVE' THEN 0 WHEN 'PAUSED' THEN 0 WHEN 'CHECKIN' THEN 1 ELSE 2 END, starts_at ASC, id DESC LIMIT 1`, installationID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// CurrentForServer is Current keyed by the game server (the Discord side knows the server, not
// the installation).
func (r *TournamentRepository) CurrentForServer(ctx context.Context, serverID int64) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT id FROM tournaments WHERE server_id=$1 AND status IN ('SIGNUP','CHECKIN','LIVE','PAUSED')
ORDER BY CASE status WHEN 'LIVE' THEN 0 WHEN 'PAUSED' THEN 0 WHEN 'CHECKIN' THEN 1 ELSE 2 END, starts_at ASC, id DESC LIMIT 1`, serverID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// TournamentTarget is an installation's binding as a tournament needs it.
type TournamentTarget struct {
	InstallationID int64
	OrganizationID int64
	GuildID        int64
	ServerID       int64
	ServerName     string
	Platform       string
	DiscordGuildID string
	DiscordInvite  string
	MapKey         string
}

// Target resolves an installation to its guild and server; nil when it has no server.
func (r *TournamentRepository) Target(ctx context.Context, installationID int64) (*TournamentTarget, error) {
	var t TournamentTarget
	err := r.pool.QueryRow(ctx, `
SELECT i.id, i.organization_id, c.guild_id, gs.id, COALESCE(gs.display_name,''), COALESCE(gs.platform,''), COALESCE(g.discord_guild_id,''),
       COALESCE((SELECT network_discord_invite_url FROM installation_feature_settings s WHERE s.installation_id=i.id), ''),
       COALESCE((SELECT map_key FROM shop_delivery_settings s WHERE s.installation_id=i.id), '')
FROM installations i
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
JOIN guilds g ON g.id = c.guild_id
JOIN game_servers gs ON gs.id = i.game_server_id
WHERE i.id = $1`, installationID).Scan(&t.InstallationID, &t.OrganizationID, &t.GuildID, &t.ServerID, &t.ServerName, &t.Platform, &t.DiscordGuildID, &t.DiscordInvite, &t.MapKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// TargetForServer is Target keyed by the game server (the first installation backed by it).
func (r *TournamentRepository) TargetForServer(ctx context.Context, serverID int64) (*TournamentTarget, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT id FROM installations WHERE game_server_id=$1 ORDER BY id LIMIT 1`, serverID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r.Target(ctx, id)
}

// OnlineCount is how many players are connected to the server right now.
func (r *TournamentRepository) OnlineCount(ctx context.Context, guildID, serverID int64) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM player_server_activity WHERE guild_id=$1 AND server_id=$2 AND currently_connected`, guildID, serverID).Scan(&n)
	return n, err
}

// Present reports which of the players are connected to the server.
func (r *TournamentRepository) Present(ctx context.Context, guildID, serverID int64, players []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(players) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT player_id FROM player_server_activity WHERE guild_id=$1 AND server_id=$2 AND currently_connected AND player_id = ANY($3)`, guildID, serverID, players)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// FighterStats is a player's server figures for the live page.
func (r *TournamentRepository) FighterStats(ctx context.Context, guildID, serverID int64, players []int64) (map[int64]tournament.Stats, error) {
	out := map[int64]tournament.Stats{}
	if len(players) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
SELECT p.id,
  (SELECT COUNT(*) FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id=p.id)::int,
  (SELECT COUNT(*) FROM deaths WHERE guild_id=$1 AND server_id=$2 AND player_id=p.id)::int,
  (SELECT COUNT(*) FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id=p.id AND headshot)::int,
  (SELECT COALESCE(MAX(distance),0) FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id=p.id)::float8,
  COALESCE((SELECT COALESCE(NULLIF(k.weapon_display,''),NULLIF(k.weapon_raw,''),'') FROM kills k WHERE k.guild_id=$1 AND k.server_id=$2 AND k.killer_player_id=p.id
            GROUP BY 1 ORDER BY COUNT(*) DESC, 1 LIMIT 1), '')
FROM players p WHERE p.guild_id=$1 AND p.id = ANY($3)`, guildID, serverID, players)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var s tournament.Stats
		if err := rows.Scan(&id, &s.Kills, &s.Deaths, &s.Headshots, &s.LongestKillMeters, &s.FavouriteWeapon); err != nil {
			return nil, err
		}
		out[id] = s
	}
	return out, rows.Err()
}

// PlayerFactions is the Faction Hub faction (tag, name, colour) of each player's verified link
// holder on the installation.
func (r *TournamentRepository) PlayerFactions(ctx context.Context, installationID, guildID int64, players []int64) (map[int64]tournament.FactionDTO, error) {
	out := map[int64]tournament.FactionDTO{}
	if len(players) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
SELECT pl.player_id, f.tag, f.name, COALESCE(f.primary_color,'') FROM player_links pl
JOIN app_users u ON u.discord_user_id = pl.discord_user_id
JOIN hub_faction_members m ON m.user_id = u.id AND m.installation_id = $1
JOIN hub_factions f ON f.id = m.faction_id
WHERE pl.guild_id=$2 AND pl.status='VERIFIED' AND pl.player_id = ANY($3)`, installationID, guildID, players)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var f tournament.FactionDTO
		if err := rows.Scan(&id, &f.Tag, &f.Name, &f.Color); err != nil {
			return nil, err
		}
		out[id] = f
	}
	return out, rows.Err()
}

// RankedRP is the RP of each player in the server's active Ranked season (absent: 0).
func (r *TournamentRepository) RankedRP(ctx context.Context, guildID, serverID int64, players []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	if len(players) == 0 {
		return out, nil
	}
	keys := make([]string, 0, len(players))
	for _, p := range players {
		keys = append(keys, fmt.Sprint(p))
	}
	rows, err := r.pool.Query(ctx, `
SELECT a.attacker_key, COALESCE(SUM(a.amount),0)::bigint FROM ranked_awards a
JOIN ranked_seasons s ON s.id=a.season_id AND s.scope='SERVER' AND s.status='ACTIVE' AND s.server_id=$1
WHERE a.outcome='AWARDED' AND a.attacker_key = ANY($2) GROUP BY a.attacker_key`, serverID, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var rp int64
		if err := rows.Scan(&key, &rp); err != nil {
			return nil, err
		}
		var id int64
		if _, err := fmt.Sscan(strings.TrimSpace(key), &id); err == nil {
			out[id] = rp
		}
	}
	return out, rows.Err()
}

// LinkedPlayer is the VERIFIED player of a Discord user on the guild (0 when none), with the
// player's display name.
func (r *TournamentRepository) LinkedPlayer(ctx context.Context, guildID int64, discordUserID string) (int64, string, error) {
	var id int64
	var name string
	err := r.pool.QueryRow(ctx, `SELECT p.id, p.display_name FROM player_links pl JOIN players p ON p.id=pl.player_id
WHERE pl.guild_id=$1 AND pl.discord_user_id=$2 AND pl.status='VERIFIED'`, guildID, discordUserID).Scan(&id, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", nil
	}
	return id, name, err
}

// EventsChannel is the installation's EVENTS route channel for the server ("" when none).
func (r *TournamentRepository) EventsChannel(ctx context.Context, installationID int64) (string, error) {
	var ch string
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(channel_id,'') FROM installation_channel_routes WHERE installation_id=$1 AND route_key='EVENTS'`, installationID).Scan(&ch)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return ch, err
}

// LatestDraftForServer is the server's newest DRAFT tournament (0 when none).
func (r *TournamentRepository) LatestDraftForServer(ctx context.Context, serverID int64) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT id FROM tournaments WHERE server_id=$1 AND status='DRAFT' ORDER BY created_at DESC, id DESC LIMIT 1`, serverID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}
