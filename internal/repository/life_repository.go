package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Life causes. ADM logs a death one of two ways (DayZ's PluginAdminLog.PlayerKilled): "killed by
// Player ..." when another player's weapon did it, which is persisted as a kill, or "died. Stats> ..."
// when nothing else did (bleeding out, starvation, a suicide), which is persisted as a death. PVP is
// only ever set from a persisted kill naming this player as the victim; SUICIDE only from a persisted
// suicide emote inside the same life; everything else is OTHER - the "died" line carries no cause and
// none is inferred. A death ADM logs as "killed by <non-player>" (infected, animals) is not parsed at
// all today, so it does not end a life here either.
const (
	LifeCausePVP     = "PVP"
	LifeCauseSuicide = "SUICIDE"
	LifeCauseOther   = "OTHER"
)

// lifeSuicideWindow is how far before a "died" line its suicide emote may sit. ADM writes them
// seconds apart; the window only has to survive second-resolution clocks and the ingestion fallback
// to wall time.
const lifeSuicideWindow = 60 * time.Second

// lifeDuplicateWindow rejects a second life ending for the same player within a few seconds of the
// previous one: a replayed line whose fingerprint differs, or a "died" line trailing a "killed by"
// line for the same death - never a real second death.
const lifeDuplicateWindow = 10 * time.Second

// LifeRepository records and reads lives (docs/LIVES.md): the stretch between two deaths of one
// player on one server.
type LifeRepository struct{ pool *pgxpool.Pool }

func NewLifeRepository(pool *pgxpool.Pool) *LifeRepository { return &LifeRepository{pool: pool} }

// Life is one ended life.
type Life struct {
	ID              int64
	GuildID         int64
	ServerID        int64
	PlayerID        int64
	PlayerName      string
	StartedAt       time.Time
	EndedAt         time.Time
	PlaytimeSeconds *int64 // nil: the life began before lives were recorded
	Kills           int
	Headshots       int
	LongestKillM    *float64
	TrackedDistance *float64 // nil: fewer than two location samples in the life
	LocationSamples int
	Cause           string
	KillerPlayerID  *int64
	KillerName      string
	KillID          *int64
	Weapon          string
	DistanceM       *float64
}

// LifeEndInput is one durably persisted death: a death row, or a kill row whose victim is PlayerID.
type LifeEndInput struct {
	GuildID     int64
	ServerID    int64
	PlayerID    int64
	SeasonID    *int64
	At          time.Time
	Fingerprint string // the persisted row's own fingerprint
	// Kill is set when a persisted kill ended the life.
	Kill *LifeEndKill
}

// LifeEndKill is the persisted kill that ended a life.
type LifeEndKill struct {
	KillID         int64
	KillerPlayerID int64
	Weapon         string
	Distance       *float64
}

// RecordDeath closes the player's current life at in.At and returns it. It returns (nil, nil) when
// nothing was recorded: the death is already recorded, or it lands within lifeDuplicateWindow of the
// previous one.
func (r *LifeRepository) RecordDeath(ctx context.Context, in LifeEndInput) (*Life, error) {
	if in.GuildID <= 0 || in.ServerID <= 0 || in.PlayerID <= 0 || in.Fingerprint == "" {
		return nil, nil
	}
	at := in.At.UTC()

	// Where the life began, and the playtime mark taken there.
	var prevEnd *time.Time
	var prevMark *int64
	err := r.pool.QueryRow(ctx, `SELECT ended_at, playtime_mark FROM player_lives WHERE server_id=$1 AND player_id=$2 ORDER BY ended_at DESC LIMIT 1`,
		in.ServerID, in.PlayerID).Scan(&prevEnd, &prevMark)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("previous life: %w", err)
	}
	if prevEnd != nil && at.Sub(*prevEnd) < lifeDuplicateWindow {
		return nil, nil
	}

	// The running playtime total at the moment of death (the next life's mark).
	var total int64
	var firstSeen *time.Time
	err = r.pool.QueryRow(ctx, `
SELECT total_observed_seconds + CASE WHEN currently_connected AND last_observed_at IS NOT NULL AND $4::timestamptz >= last_observed_at
            AND EXTRACT(EPOCH FROM ($4::timestamptz - last_observed_at)) <= 300
       THEN EXTRACT(EPOCH FROM ($4::timestamptz - last_observed_at))::BIGINT ELSE 0 END, first_seen_at
FROM player_server_activity WHERE guild_id=$1 AND server_id=$2 AND player_id=$3`,
		in.GuildID, in.ServerID, in.PlayerID, at).Scan(&total, &firstSeen)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("playtime mark: %w", err)
	}

	life := Life{GuildID: in.GuildID, ServerID: in.ServerID, PlayerID: in.PlayerID, EndedAt: at, Cause: LifeCauseOther}
	switch {
	case prevEnd != nil:
		life.StartedAt = *prevEnd
		played := total - *prevMark
		if played < 0 {
			played = 0
		}
		life.PlaytimeSeconds = &played
	default:
		// No recorded life yet. An earlier death on this server means the life began at that death,
		// before any mark existed (playtime unknown). No earlier death means this is the player's
		// first life here, which began when they were first seen and spans all their playtime.
		var lastDeath *time.Time
		var thisKill int64
		if in.Kill != nil {
			thisKill = in.Kill.KillID
		}
		if err := r.pool.QueryRow(ctx, `
SELECT MAX(at) FROM (
    SELECT COALESCE(event_time, created_at) AS at FROM deaths WHERE guild_id=$1 AND server_id=$2 AND player_id=$3 AND event_fingerprint<>$4
    UNION ALL
    SELECT COALESCE(event_time, created_at) FROM kills WHERE guild_id=$1 AND server_id=$2 AND victim_player_id=$3 AND id<>$5
) d WHERE at < $6::timestamptz`,
			in.GuildID, in.ServerID, in.PlayerID, in.Fingerprint, thisKill, at.Add(-lifeDuplicateWindow)).Scan(&lastDeath); err != nil {
			return nil, fmt.Errorf("previous death: %w", err)
		}
		switch {
		case lastDeath != nil:
			life.StartedAt = *lastDeath
		case firstSeen != nil && firstSeen.Before(at):
			life.StartedAt = *firstSeen
			played := total
			life.PlaytimeSeconds = &played
		default:
			life.StartedAt = at
			played := total
			life.PlaytimeSeconds = &played
		}
	}

	// Cause: the persisted kill that ended the life, else a suicide emote inside it.
	if k := in.Kill; k != nil {
		life.Cause, life.Weapon, life.DistanceM = LifeCausePVP, k.Weapon, k.Distance
		if k.KillID > 0 {
			life.KillID = &k.KillID
		}
		if k.KillerPlayerID > 0 {
			life.KillerPlayerID = &k.KillerPlayerID
		}
	} else {
		var suicide bool
		if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deaths WHERE guild_id=$1 AND server_id=$2 AND player_id=$3 AND death_type=$4
  AND COALESCE(event_time, created_at) > $5::timestamptz AND COALESCE(event_time, created_at) BETWEEN $6::timestamptz AND $7::timestamptz)`,
			in.GuildID, in.ServerID, in.PlayerID, DeathTypeSuicide, life.StartedAt, at.Add(-lifeSuicideWindow), at.Add(5*time.Second)).Scan(&suicide); err != nil {
			return nil, fmt.Errorf("suicide lookup: %w", err)
		}
		if suicide {
			life.Cause = LifeCauseSuicide
		}
	}

	// What the player did with the life.
	if err := r.pool.QueryRow(ctx, `
SELECT COUNT(*), COUNT(*) FILTER (WHERE headshot), MAX(distance) FROM kills
WHERE guild_id=$1 AND server_id=$2 AND killer_player_id=$3 AND victim_player_id IS DISTINCT FROM $3
  AND COALESCE(event_time, created_at) > $4::timestamptz AND COALESCE(event_time, created_at) <= $5::timestamptz`,
		in.GuildID, in.ServerID, in.PlayerID, life.StartedAt, at).Scan(&life.Kills, &life.Headshots, &life.LongestKillM); err != nil {
		return nil, fmt.Errorf("life kills: %w", err)
	}
	if err := r.pool.QueryRow(ctx, lifeDistanceSQL, in.ServerID, in.PlayerID, life.StartedAt, at).Scan(&life.TrackedDistance, &life.LocationSamples); err != nil {
		return nil, fmt.Errorf("life distance: %w", err)
	}

	err = r.pool.QueryRow(ctx, `
INSERT INTO player_lives(guild_id, server_id, player_id, season_id, started_at, ended_at, playtime_seconds, playtime_mark,
    kills, headshots, longest_kill_m, tracked_distance_m, location_samples, cause, killer_player_id, kill_id, weapon, distance_m, death_fingerprint)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,NULLIF($17,''),$18,$19)
ON CONFLICT (guild_id, death_fingerprint) DO NOTHING RETURNING id`,
		in.GuildID, in.ServerID, in.PlayerID, in.SeasonID, life.StartedAt, at, life.PlaytimeSeconds, total,
		life.Kills, life.Headshots, life.LongestKillM, life.TrackedDistance, life.LocationSamples, life.Cause,
		life.KillerPlayerID, life.KillID, life.Weapon, life.DistanceM, in.Fingerprint).Scan(&life.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("insert life: %w", err)
	}
	return &life, nil
}

// lifeDistanceSQL sums the straight-line distance between consecutive location samples of one
// player on one server inside ($3, $4]. Location history older than the retention window is gone,
// so a very long life is measured over what remains.
const lifeDistanceSQL = `
SELECT SUM(step), COUNT(*)::int FROM (
    SELECT sqrt(power(x - LAG(x) OVER w, 2) + power(z - LAG(z) OVER w, 2)) AS step
    FROM player_location_events
    WHERE server_id=$1 AND player_id=$2 AND observed_at > $3::timestamptz AND observed_at <= $4::timestamptz
    WINDOW w AS (ORDER BY observed_at, id)
) s`

const lifeColumns = `l.id, l.guild_id, l.server_id, l.player_id, p.display_name, l.started_at, l.ended_at, l.playtime_seconds,
    l.kills, l.headshots, l.longest_kill_m, l.tracked_distance_m, l.location_samples, l.cause,
    l.killer_player_id, COALESCE(k.display_name, ''), l.kill_id, COALESCE(l.weapon, ''), l.distance_m`

const lifeFrom = ` FROM player_lives l JOIN players p ON p.id = l.player_id LEFT JOIN players k ON k.id = l.killer_player_id `

func scanLife(row pgx.Row) (Life, error) {
	var l Life
	err := row.Scan(&l.ID, &l.GuildID, &l.ServerID, &l.PlayerID, &l.PlayerName, &l.StartedAt, &l.EndedAt, &l.PlaytimeSeconds,
		&l.Kills, &l.Headshots, &l.LongestKillM, &l.TrackedDistance, &l.LocationSamples, &l.Cause,
		&l.KillerPlayerID, &l.KillerName, &l.KillID, &l.Weapon, &l.DistanceM)
	return l, err
}

func (r *LifeRepository) list(ctx context.Context, q string, args ...any) ([]Life, error) {
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Life{}
	for rows.Next() {
		l, err := scanLife(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Get returns one life scoped to its guild and server, or (nil, nil).
func (r *LifeRepository) Get(ctx context.Context, guildID, serverID, lifeID int64) (*Life, error) {
	l, err := scanLife(r.pool.QueryRow(ctx, `SELECT `+lifeColumns+lifeFrom+` WHERE l.guild_id=$1 AND l.server_id=$2 AND l.id=$3`, guildID, serverID, lifeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// PlayerLives returns a player's most recently ended lives on one server.
func (r *LifeRepository) PlayerLives(ctx context.Context, guildID, serverID, playerID int64, limit int) ([]Life, error) {
	return r.list(ctx, `SELECT `+lifeColumns+lifeFrom+` WHERE l.guild_id=$1 AND l.server_id=$2 AND l.player_id=$3 ORDER BY l.ended_at DESC, l.id DESC LIMIT $4`,
		guildID, serverID, playerID, clampLimit(limit, 10, 50))
}

// Life leaderboard metrics.
const (
	LifeMetricPlaytime = "PLAYTIME"
	LifeMetricKills    = "KILLS"
	LifeMetricDistance = "DISTANCE"
)

// TopLives ranks ended lives on one server by metric, optionally inside [since, +inf). A life whose
// metric was never measured is not ranked.
func (r *LifeRepository) TopLives(ctx context.Context, guildID, serverID int64, metric string, since *time.Time, limit int) ([]Life, error) {
	var where, order string
	switch metric {
	case LifeMetricPlaytime:
		where, order = "l.playtime_seconds IS NOT NULL AND l.playtime_seconds > 0", "l.playtime_seconds DESC"
	case LifeMetricKills:
		where, order = "l.kills > 0", "l.kills DESC, l.playtime_seconds ASC NULLS LAST"
	case LifeMetricDistance:
		where, order = "l.tracked_distance_m IS NOT NULL AND l.tracked_distance_m > 0", "l.tracked_distance_m DESC"
	default:
		return nil, fmt.Errorf("unknown life metric %q", metric)
	}
	return r.list(ctx, `SELECT `+lifeColumns+lifeFrom+` WHERE l.guild_id=$1 AND l.server_id=$2 AND ($3::timestamptz IS NULL OR l.ended_at >= $3) AND `+where+
		` ORDER BY `+order+`, l.id LIMIT $4`, guildID, serverID, since, clampLimit(limit, 10, 50))
}

// CurrentLife is a life still in progress.
type CurrentLife struct {
	PlayerID        int64
	PlayerName      string
	StartedAt       time.Time
	PlaytimeSeconds *int64 // nil: began before lives were recorded
	Kills           int
	Online          bool
}

// currentLifeSQL derives every player's in-progress life on one server: it began at their last
// recorded life's end (or, with no death at all, when they were first seen) and its playtime is the
// running total minus the mark taken there. A player whose last death predates player_lives has an
// unknown playtime, exactly as RecordDeath records it.
const currentLifeSQL = `
SELECT a.player_id, p.display_name,
       COALESCE(last.ended_at, d.last_death, a.first_seen_at) AS started_at,
       CASE WHEN last.ended_at IS NOT NULL THEN GREATEST(a.total_observed_seconds + live.extra - last.playtime_mark, 0)
            WHEN d.last_death IS NULL THEN a.total_observed_seconds + live.extra
       END AS playtime_seconds,
       (SELECT COUNT(*) FROM kills k WHERE k.guild_id=a.guild_id AND k.server_id=a.server_id AND k.killer_player_id=a.player_id
           AND k.victim_player_id IS DISTINCT FROM a.player_id
           AND COALESCE(k.event_time, k.created_at) > COALESCE(last.ended_at, d.last_death, a.first_seen_at))::int AS kills,
       a.currently_connected
FROM player_server_activity a
JOIN players p ON p.id = a.player_id
LEFT JOIN LATERAL (SELECT ended_at, playtime_mark FROM player_lives l WHERE l.server_id=a.server_id AND l.player_id=a.player_id ORDER BY ended_at DESC LIMIT 1) last ON TRUE
LEFT JOIN LATERAL (SELECT MAX(at) AS last_death FROM (
        SELECT COALESCE(event_time, created_at) AS at FROM deaths dd WHERE dd.guild_id=a.guild_id AND dd.server_id=a.server_id AND dd.player_id=a.player_id
        UNION ALL
        SELECT COALESCE(event_time, created_at) FROM kills kk WHERE kk.guild_id=a.guild_id AND kk.server_id=a.server_id AND kk.victim_player_id=a.player_id
    ) x) d ON TRUE
CROSS JOIN LATERAL (SELECT CASE WHEN a.currently_connected AND a.last_observed_at IS NOT NULL AND $3::timestamptz >= a.last_observed_at
        AND EXTRACT(EPOCH FROM ($3::timestamptz - a.last_observed_at)) <= 300
        THEN EXTRACT(EPOCH FROM ($3::timestamptz - a.last_observed_at))::BIGINT ELSE 0 END AS extra) live
WHERE a.guild_id=$1 AND a.server_id=$2`

func scanCurrentLife(row pgx.Row) (CurrentLife, error) {
	var c CurrentLife
	err := row.Scan(&c.PlayerID, &c.PlayerName, &c.StartedAt, &c.PlaytimeSeconds, &c.Kills, &c.Online)
	return c, err
}

// CurrentLife returns the player's in-progress life, or (nil, nil) if they were never observed on
// the server.
func (r *LifeRepository) CurrentLife(ctx context.Context, guildID, serverID, playerID int64, now time.Time) (*CurrentLife, error) {
	c, err := scanCurrentLife(r.pool.QueryRow(ctx, currentLifeSQL+` AND a.player_id=$4`, guildID, serverID, now.UTC(), playerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// LongestAlive ranks in-progress lives with a measured playtime, longest first - the players who
// have gone the longest without dying. Only players seen since activeSince are ranked, so a player
// who stopped playing months ago does not hold the top spot forever.
func (r *LifeRepository) LongestAlive(ctx context.Context, guildID, serverID int64, now, activeSince time.Time, limit int) ([]CurrentLife, error) {
	rows, err := r.pool.Query(ctx, `SELECT * FROM (`+currentLifeSQL+` AND a.last_seen_at >= $4::timestamptz) c WHERE playtime_seconds IS NOT NULL AND playtime_seconds > 0 ORDER BY playtime_seconds DESC, player_id LIMIT $5`,
		guildID, serverID, now.UTC(), activeSince.UTC(), clampLimit(limit, 10, 50))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CurrentLife{}
	for rows.Next() {
		c, err := scanCurrentLife(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LifeSummary aggregates a player's ended lives on one server.
type LifeSummary struct {
	Lives              int
	LongestPlaytime    *int64
	AveragePlaytime    *float64
	MostKills          int
	TotalTrackedMeters float64
	DeathsByPVP        int
	DeathsBySuicide    int
	DeathsByOther      int
}

func (r *LifeRepository) Summary(ctx context.Context, guildID, serverID, playerID int64) (LifeSummary, error) {
	var s LifeSummary
	err := r.pool.QueryRow(ctx, `
SELECT COUNT(*)::int, MAX(playtime_seconds), AVG(playtime_seconds)::float8, COALESCE(MAX(kills), 0)::int, COALESCE(SUM(tracked_distance_m), 0)::float8,
       COUNT(*) FILTER (WHERE cause='PVP')::int, COUNT(*) FILTER (WHERE cause='SUICIDE')::int, COUNT(*) FILTER (WHERE cause='OTHER')::int
FROM player_lives WHERE guild_id=$1 AND server_id=$2 AND player_id=$3`, guildID, serverID, playerID).Scan(
		&s.Lives, &s.LongestPlaytime, &s.AveragePlaytime, &s.MostKills, &s.TotalTrackedMeters, &s.DeathsByPVP, &s.DeathsBySuicide, &s.DeathsByOther)
	return s, err
}

// SetDeathRecap turns the death recap DM on or off for one Discord user in one guild.
func (r *LifeRepository) SetDeathRecap(ctx context.Context, guildID int64, discordUserID string, on bool) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO player_recap_prefs(guild_id, discord_user_id, death_recap) VALUES($1,$2,$3)
ON CONFLICT (guild_id, discord_user_id) DO UPDATE SET death_recap=EXCLUDED.death_recap, updated_at=NOW()`, guildID, discordUserID, on)
	return err
}

// DeathRecapEnabled reports the Discord user's current choice (false when they never chose).
func (r *LifeRepository) DeathRecapEnabled(ctx context.Context, guildID int64, discordUserID string) (bool, error) {
	var on bool
	err := r.pool.QueryRow(ctx, `SELECT death_recap FROM player_recap_prefs WHERE guild_id=$1 AND discord_user_id=$2`, guildID, discordUserID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return on, err
}

// DeathRecapRecipient returns the Discord user to DM about playerID's death: the holder of the
// player's VERIFIED link, if they opted in - or, when the server switched life recaps on for
// everyone (the lifeStoryDms automation, docs/FEATURE_UPGRADES.md), if they never turned them off.
// ok is false otherwise.
func (r *LifeRepository) DeathRecapRecipient(ctx context.Context, guildID, playerID int64) (discordUserID string, ok bool, err error) {
	err = r.pool.QueryRow(ctx, `
SELECT pl.discord_user_id FROM player_links pl
LEFT JOIN player_recap_prefs pr ON pr.guild_id = pl.guild_id AND pr.discord_user_id = pl.discord_user_id
WHERE pl.guild_id=$1 AND pl.player_id=$2 AND pl.status='VERIFIED'
  AND (pr.death_recap OR (pr.discord_user_id IS NULL AND EXISTS(
    SELECT 1 FROM upgrade_settings u JOIN game_servers gs ON gs.id=u.server_id
    WHERE gs.guild_id=$1 AND COALESCE((u.settings->>'lifeStoryDms')::BOOLEAN,FALSE))))
LIMIT 1`, guildID, playerID).Scan(&discordUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return discordUserID, err == nil, err
}

// LongestOtherLife is the player's longest finished life on the server other than lifeID, nil
// when they have none with a recorded playtime.
func (r *LifeRepository) LongestOtherLife(ctx context.Context, guildID, serverID, playerID, lifeID int64) (*int64, error) {
	var best *int64
	err := r.pool.QueryRow(ctx, `SELECT MAX(playtime_seconds) FROM player_lives
WHERE guild_id=$1 AND server_id=$2 AND player_id=$3 AND id<>$4 AND ended_at IS NOT NULL`, guildID, serverID, playerID, lifeID).Scan(&best)
	return best, err
}

func clampLimit(limit, fallback, max int) int {
	if limit <= 0 {
		return fallback
	}
	if limit > max {
		return max
	}
	return limit
}
