package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/progression"
)

// Territory control (docs/PROGRESSION.md): hub factions hold the map's named zones by out-killing
// each other inside them over a rolling window. A zone pays its holder's active members a daily
// income in Champion Points.

var ErrTerritorySettingsInvalid = errors.New("territory settings are out of range")

type TerritorySettings struct {
	Enabled      bool       `json:"enabled"`
	WindowDays   int        `json:"windowDays"`
	MinKills     int        `json:"minKills"`
	IncomePoints int64      `json:"incomePoints"`
	Announce     bool       `json:"announce"`
	UpdatedAt    *time.Time `json:"updatedAt"`
}

func DefaultTerritorySettings() TerritorySettings {
	return TerritorySettings{WindowDays: 7, MinKills: 3, IncomePoints: 100, Announce: true}
}

func (s TerritorySettings) Validate() error {
	if s.WindowDays < 1 || s.WindowDays > 14 || s.MinKills < 1 || s.MinKills > 50 || s.IncomePoints < 0 || s.IncomePoints > 1_000_000 {
		return ErrTerritorySettingsInvalid
	}
	return nil
}

// TerritoryMemberActiveDays is how recently a faction member must have played to share the income.
const TerritoryMemberActiveDays = 7

type TerritoryRepository struct{ pool *pgxpool.Pool }

func NewTerritoryRepository(pool *pgxpool.Pool) *TerritoryRepository {
	return &TerritoryRepository{pool: pool}
}

func (r *TerritoryRepository) Settings(ctx context.Context, serverID int64) (TerritorySettings, error) {
	s := DefaultTerritorySettings()
	var updated time.Time
	err := r.pool.QueryRow(ctx, `SELECT enabled,window_days,min_kills,income_points,announce,updated_at FROM territory_settings WHERE server_id=$1`, serverID).
		Scan(&s.Enabled, &s.WindowDays, &s.MinKills, &s.IncomePoints, &s.Announce, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultTerritorySettings(), nil
	}
	if err != nil {
		return s, err
	}
	s.UpdatedAt = &updated
	return s, nil
}

func (r *TerritoryRepository) SaveSettings(ctx context.Context, serverID int64, s TerritorySettings, by string, now time.Time) (TerritorySettings, error) {
	if err := s.Validate(); err != nil {
		return s, err
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO territory_settings(server_id,enabled,window_days,min_kills,income_points,announce,updated_at,updated_by)
VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (server_id) DO UPDATE SET enabled=EXCLUDED.enabled,window_days=EXCLUDED.window_days,
min_kills=EXCLUDED.min_kills,income_points=EXCLUDED.income_points,announce=EXCLUDED.announce,updated_at=EXCLUDED.updated_at,updated_by=EXCLUDED.updated_by`,
		serverID, s.Enabled, s.WindowDays, s.MinKills, s.IncomePoints, s.Announce, now, by)
	if err != nil {
		return s, err
	}
	s.UpdatedAt = &now
	return s, nil
}

// TerritoryServer is a server with territory control on.
type TerritoryServer struct {
	GuildID, ServerID, InstallationID int64
	MapKey                            string
	Settings                          TerritorySettings
}

const territoryServerSQL = `SELECT gs.guild_id,gs.id,COALESCE(i.id,0),COALESCE(sd.map_key,''),t.enabled,t.window_days,t.min_kills,t.income_points,t.announce
FROM territory_settings t JOIN game_servers gs ON gs.id=t.server_id
LEFT JOIN installations i ON i.game_server_id=gs.id
LEFT JOIN shop_delivery_settings sd ON sd.installation_id=i.id `

func scanTerritoryServer(row pgx.Row) (TerritoryServer, error) {
	var s TerritoryServer
	err := row.Scan(&s.GuildID, &s.ServerID, &s.InstallationID, &s.MapKey, &s.Settings.Enabled, &s.Settings.WindowDays, &s.Settings.MinKills,
		&s.Settings.IncomePoints, &s.Settings.Announce)
	return s, err
}

// EnabledServers lists the guild's servers with territory control on.
func (r *TerritoryRepository) EnabledServers(ctx context.Context, guildID int64) ([]TerritoryServer, error) {
	rows, err := r.pool.Query(ctx, territoryServerSQL+`WHERE gs.guild_id=$1 AND t.enabled ORDER BY gs.id`, guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TerritoryServer
	for rows.Next() {
		s, err := scanTerritoryServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Server returns one server's territory facts (settings default when never saved).
func (r *TerritoryRepository) Server(ctx context.Context, serverID int64) (TerritoryServer, error) {
	s, err := scanTerritoryServer(r.pool.QueryRow(ctx, territoryServerSQL+`WHERE gs.id=$1`, serverID))
	if errors.Is(err, pgx.ErrNoRows) {
		s = TerritoryServer{ServerID: serverID, Settings: DefaultTerritorySettings()}
		err = r.pool.QueryRow(ctx, `SELECT gs.guild_id,COALESCE(i.id,0),COALESCE(sd.map_key,'') FROM game_servers gs
LEFT JOIN installations i ON i.game_server_id=gs.id LEFT JOIN shop_delivery_settings sd ON sd.installation_id=i.id WHERE gs.id=$1`, serverID).
			Scan(&s.GuildID, &s.InstallationID, &s.MapKey)
	}
	return s, err
}

// RecentKills returns the server's PvP kills since `from` with where they happened (the killer's
// position, else the victim's).
func (r *TerritoryRepository) RecentKills(ctx context.Context, guildID, serverID int64, from, to time.Time, limit int) ([]FightKill, error) {
	return (&FightRepository{pool: r.pool}).RecentKills(ctx, guildID, serverID, from, to, limit)
}

// TerritoryKill is a PvP kill placed in a zone.
type TerritoryKill struct {
	KillID   int64
	ZoneKey  string
	KillerID int64
	VictimID int64
	At       time.Time
}

// verifiedHubMember is the hub faction of a player on the installation, counting a member only while
// their DayZ link to that player is verified (the live map's rule).
const verifiedHubMember = `(SELECT m.faction_id FROM hub_faction_members m WHERE m.installation_id=$2 AND m.player_id=%s
AND EXISTS (SELECT 1 FROM app_users vu JOIN player_links vl ON vl.discord_user_id=vu.discord_user_id AND vl.guild_id=$3
  WHERE vu.id=m.user_id AND vl.player_id=m.player_id AND vl.status='VERIFIED') LIMIT 1)`

// RecordKills stores kills placed in zones with the factions of both players now. A kill already
// stored keeps its first factions.
func (r *TerritoryRepository) RecordKills(ctx context.Context, s TerritoryServer, kills []TerritoryKill) (int, error) {
	if len(kills) == 0 {
		return 0, nil
	}
	ids, zones, killers, victims, ats := make([]int64, len(kills)), make([]string, len(kills)), make([]int64, len(kills)), make([]int64, len(kills)), make([]time.Time, len(kills))
	for i, k := range kills {
		ids[i], zones[i], killers[i], victims[i], ats[i] = k.KillID, k.ZoneKey, k.KillerID, k.VictimID, k.At
	}
	tag, err := r.pool.Exec(ctx, `INSERT INTO territory_kills(kill_id,server_id,zone_key,killer_player_id,victim_player_id,faction_id,victim_faction_id,at)
SELECT x.kill_id,$1,x.zone_key,x.killer,x.victim,`+sprintfMember("x.killer")+`,`+sprintfMember("x.victim")+`,x.at
FROM UNNEST($4::BIGINT[],$5::TEXT[],$6::BIGINT[],$7::BIGINT[],$8::TIMESTAMPTZ[]) AS x(kill_id,zone_key,killer,victim,at)
ON CONFLICT (kill_id) DO NOTHING`, s.ServerID, s.InstallationID, s.GuildID, ids, zones, killers, victims, ats)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// sprintfMember is verifiedHubMember for the player in col.
func sprintfMember(col string) string { return strings.ReplaceAll(verifiedHubMember, "%s", col) }

// Standings returns each zone's faction points since `since`: kills by a faction's members on
// players outside their faction, the same killer and victim counting once an hour.
func (r *TerritoryRepository) Standings(ctx context.Context, serverID int64, since time.Time) (map[string][]progression.FactionScore, error) {
	rows, err := r.pool.Query(ctx, `SELECT zone_key,faction_id,COUNT(DISTINCT (killer_player_id,victim_player_id,date_trunc('hour',at)))::INT FROM territory_kills
WHERE server_id=$1 AND at>=$2 AND faction_id IS NOT NULL AND faction_id IS DISTINCT FROM victim_faction_id
GROUP BY zone_key,faction_id`, serverID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]progression.FactionScore{}
	for rows.Next() {
		var zone string
		var fs progression.FactionScore
		if err := rows.Scan(&zone, &fs.FactionID, &fs.Points); err != nil {
			return nil, err
		}
		out[zone] = append(out[zone], fs)
	}
	return out, rows.Err()
}

// TerritoryHold is a zone held by a faction.
type TerritoryHold struct {
	ZoneKey   string
	FactionID int64
	Since     time.Time
}

// Holds returns the server's held zones by zone key.
func (r *TerritoryRepository) Holds(ctx context.Context, serverID int64) (map[string]TerritoryHold, error) {
	rows, err := r.pool.Query(ctx, `SELECT zone_key,faction_id,since FROM territory_holds WHERE server_id=$1 AND until IS NULL`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TerritoryHold{}
	for rows.Next() {
		var h TerritoryHold
		if err := rows.Scan(&h.ZoneKey, &h.FactionID, &h.Since); err != nil {
			return nil, err
		}
		out[h.ZoneKey] = h
	}
	return out, rows.Err()
}

// ChangeHolder closes the zone's current hold (if it is still `from`) and opens one for `to` (0 =
// the zone goes neutral). false when the zone changed in the meantime.
func (r *TerritoryRepository) ChangeHolder(ctx context.Context, serverID int64, zone string, from, to int64, now time.Time) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `UPDATE territory_holds SET until=$4 WHERE server_id=$1 AND zone_key=$2 AND until IS NULL AND faction_id=$3`, serverID, zone, from, now)
	if err != nil {
		return false, err
	}
	if from != 0 && tag.RowsAffected() == 0 {
		return false, nil
	}
	if to != 0 {
		var prev *int64
		if from != 0 {
			prev = &from
		}
		if _, err := tx.Exec(ctx, `INSERT INTO territory_holds(server_id,zone_key,faction_id,previous_faction_id,since,announced_at) VALUES($1,$2,$3,$4,$5,$5)`,
			serverID, zone, to, prev, now); err != nil {
			if isUniqueViolation(err) {
				return false, nil
			}
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

// TerritoryFaction is a hub faction as territory shows it.
type TerritoryFaction struct {
	ID    int64   `json:"id"`
	Name  string  `json:"name"`
	Tag   string  `json:"tag"`
	Color *string `json:"color"`
}

// Factions returns the named hub factions.
func (r *TerritoryRepository) Factions(ctx context.Context, ids []int64) (map[int64]TerritoryFaction, error) {
	out := map[int64]TerritoryFaction{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT id,name,tag,primary_color FROM hub_factions WHERE id=ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f TerritoryFaction
		if err := rows.Scan(&f.ID, &f.Name, &f.Tag, &f.Color); err != nil {
			return nil, err
		}
		out[f.ID] = f
	}
	return out, rows.Err()
}

// PlayerFaction returns the verified player's hub faction on the installation (0 when none).
func (r *TerritoryRepository) PlayerFaction(ctx context.Context, s TerritoryServer, playerID int64) (int64, error) {
	var id *int64
	err := r.pool.QueryRow(ctx, `SELECT `+sprintfMember("$1::BIGINT")+``, playerID, s.InstallationID, s.GuildID).Scan(&id)
	if err != nil || id == nil {
		return 0, err
	}
	return *id, nil
}

// RecordPayout records that the zone was paid for day.
func (r *TerritoryRepository) RecordPayout(ctx context.Context, serverID int64, day time.Time, zone string, factionID int64, players int, points int64, now time.Time) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO territory_payouts(server_id,day,zone_key,faction_id,players,points,paid_at) VALUES($1,$2::DATE,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`,
		serverID, day.Format("2006-01-02"), zone, factionID, players, points, now)
	return err
}

// ZoneMember is an active, verified member of the faction holding a zone.
type ZoneMember struct {
	ZoneKey   string
	FactionID int64
	PlayerID  int64
}

// IncomeMembers lists, per held zone not paid yet for day, the holder's members who played on the
// server recently.
func (r *TerritoryRepository) IncomeMembers(ctx context.Context, s TerritoryServer, day, now time.Time) ([]ZoneMember, error) {
	rows, err := r.pool.Query(ctx, `SELECT h.zone_key,h.faction_id,m.player_id FROM territory_holds h
JOIN hub_faction_members m ON m.faction_id=h.faction_id AND m.player_id IS NOT NULL
JOIN player_server_activity a ON a.guild_id=$3 AND a.server_id=h.server_id AND a.player_id=m.player_id AND a.last_seen_at>=$4
WHERE h.server_id=$1 AND h.until IS NULL AND m.installation_id=$2
AND NOT EXISTS (SELECT 1 FROM territory_payouts tp WHERE tp.server_id=h.server_id AND tp.day=$5::DATE AND tp.zone_key=h.zone_key)
AND EXISTS (SELECT 1 FROM app_users vu JOIN player_links vl ON vl.discord_user_id=vu.discord_user_id AND vl.guild_id=$3
  WHERE vu.id=m.user_id AND vl.player_id=m.player_id AND vl.status='VERIFIED')
ORDER BY h.zone_key,m.player_id`, s.ServerID, s.InstallationID, s.GuildID, now.AddDate(0, 0, -TerritoryMemberActiveDays), day.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ZoneMember
	for rows.Next() {
		var m ZoneMember
		if err := rows.Scan(&m.ZoneKey, &m.FactionID, &m.PlayerID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// TerritoryCapture is a hold change.
type TerritoryCapture struct {
	ZoneKey     string    `json:"zone"`
	FactionID   int64     `json:"-"`
	PreviousID  *int64    `json:"-"`
	Since       time.Time `json:"at"`
	Faction     string    `json:"faction"`
	PreviousTag string    `json:"previous,omitempty"`
}

// RecentCaptures returns the server's latest hold changes, newest first.
func (r *TerritoryRepository) RecentCaptures(ctx context.Context, serverID int64, limit int) ([]TerritoryCapture, error) {
	rows, err := r.pool.Query(ctx, `SELECT h.zone_key,h.faction_id,h.previous_faction_id,h.since,COALESCE(f.name,''),COALESCE(pf.tag,'')
FROM territory_holds h LEFT JOIN hub_factions f ON f.id=h.faction_id LEFT JOIN hub_factions pf ON pf.id=h.previous_faction_id
WHERE h.server_id=$1 ORDER BY h.since DESC,h.id DESC LIMIT $2`, serverID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TerritoryCapture{}
	for rows.Next() {
		var c TerritoryCapture
		if err := rows.Scan(&c.ZoneKey, &c.FactionID, &c.PreviousID, &c.Since, &c.Faction, &c.PreviousTag); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
