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
)

// Feature upgrades (docs/FEATURE_UPGRADES.md): optional automations layered on existing features,
// each switched on per server in Client Hub → Growth → Automations. The switches live together in
// one JSON document per server; each upgrade keeps its own state in its own table.

type UpgradeRepository struct{ pool *pgxpool.Pool }

func NewUpgradeRepository(pool *pgxpool.Pool) *UpgradeRepository {
	return &UpgradeRepository{pool: pool}
}

var ErrUpgradeSettingsInvalid = errors.New("automation settings are out of range")

// UpgradeSettings are one server's automation switches. The zero value is everything off.
type UpgradeSettings struct {
	// Priority queue: the top N ranked players (0 = off, up to 50) and supporter tier holders.
	PriorityRankedTop int  `json:"priorityRankedTop"`
	PriorityVIP       bool `json:"priorityVip"`
	// Win-back DMs to players who have not played for WinbackDays (3-30).
	WinbackEnabled bool `json:"winbackEnabled"`
	WinbackDays    int  `json:"winbackDays"`
	// Champ credits for the ranked season's top 3 when it ends (0 = none for that place).
	SeasonRewards [3]int64 `json:"seasonRewards"`
	// A live standings message in Discord while a competitive event runs.
	EventScoreboard bool `json:"eventScoreboard"`
	// Ranked RP and bonus tags on kill cards.
	KillfeedRankedTags bool `json:"killfeedRankedTags"`
	// Life recap DMs for every linked player (not only those who turned them on), unless they turned them off.
	LifeStoryDMs bool `json:"lifeStoryDms"`
	// Bounty placers hear by DM when their bounty is claimed or runs out.
	BountyDMs bool `json:"bountyDms"`
	// Rent reminders three days ahead as well, to the owner's faction too.
	RentReminders bool `json:"rentReminders"`
	// Champ credits for playing each day, plus a bonus per day of streak (up to 7 days).
	DailyLoginCredits     int64 `json:"dailyLoginCredits"`
	DailyLoginStreakBonus int64 `json:"dailyLoginStreakBonus"`
	// Shop: a DM to the buyer at each delivery step.
	ShopProgressDMs bool `json:"shopProgressDms"`
	// Perk subscriptions and supporter tiers: a DM three days before they end.
	PerkReminderDMs bool `json:"perkReminderDms"`
	// C.A.S.E.: a weekly digest for staff, and appeals from players.
	CaseWeeklyDigest bool `json:"caseWeeklyDigest"`
	CaseAppeals      bool `json:"caseAppeals"`
	// Features channel: a post when an automation is switched on.
	FeatureAnnouncements bool `json:"featureAnnouncements"`

	// SwitchedOn is when each switch last went from off to on (kept by SaveSettings), so an
	// automation can leave alone what happened before it was turned on.
	SwitchedOn map[string]time.Time `json:"switchedOn,omitempty"`
	UpdatedAt  *time.Time           `json:"updatedAt,omitempty"`
}

// OnSince is when the switch went on (zero when never recorded).
func (s UpgradeSettings) OnSince(key string) time.Time {
	return s.SwitchedOn[key]
}

// DefaultUpgradeSettings is everything off, with amounts ready for when a switch is turned on.
func DefaultUpgradeSettings() UpgradeSettings {
	return UpgradeSettings{WinbackDays: 7}
}

func (s UpgradeSettings) Validate() error {
	credits := func(v int64) bool { return v >= 0 && v <= 1_000_000 }
	if s.PriorityRankedTop < 0 || s.PriorityRankedTop > 50 || s.WinbackDays < 3 || s.WinbackDays > 30 ||
		!credits(s.DailyLoginCredits) || !credits(s.DailyLoginStreakBonus) {
		return ErrUpgradeSettingsInvalid
	}
	for _, v := range s.SeasonRewards {
		if !credits(v) {
			return ErrUpgradeSettingsInvalid
		}
	}
	return nil
}

// UpgradeSwitches lists the switches that went from off to on, by their JSON name.
func UpgradeSwitches(before, after UpgradeSettings) []string {
	var b, a map[string]any
	bj, _ := json.Marshal(before)
	aj, _ := json.Marshal(after)
	_ = json.Unmarshal(bj, &b)
	_ = json.Unmarshal(aj, &a)
	on := func(v any) bool {
		switch t := v.(type) {
		case bool:
			return t
		case float64:
			return t > 0
		case []any:
			for _, x := range t {
				if f, ok := x.(float64); ok && f > 0 {
					return true
				}
			}
		}
		return false
	}
	var out []string
	for key, v := range a {
		if key == "updatedAt" || key == "switchedOn" || key == "winbackDays" || key == "dailyLoginStreakBonus" {
			continue
		}
		if on(v) && !on(b[key]) {
			out = append(out, key)
		}
	}
	return out
}

// Settings returns the server's switches (all off before they are first saved).
func (r *UpgradeRepository) Settings(ctx context.Context, serverID int64) (UpgradeSettings, error) {
	s := DefaultUpgradeSettings()
	var raw []byte
	var updated time.Time
	err := r.pool.QueryRow(ctx, `SELECT settings,updated_at FROM upgrade_settings WHERE server_id=$1`, serverID).Scan(&raw, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return DefaultUpgradeSettings(), fmt.Errorf("decode automation settings: %w", err)
	}
	if s.WinbackDays == 0 {
		s.WinbackDays = 7
	}
	s.UpdatedAt = &updated
	return s, nil
}

// SaveSettings stores the server's switches and returns the ones before.
func (r *UpgradeRepository) SaveSettings(ctx context.Context, serverID int64, s UpgradeSettings, by string, now time.Time) (UpgradeSettings, error) {
	if serverID <= 0 {
		return UpgradeSettings{}, ErrUpgradeSettingsInvalid
	}
	if err := s.Validate(); err != nil {
		return UpgradeSettings{}, err
	}
	before, err := r.Settings(ctx, serverID)
	if err != nil {
		return UpgradeSettings{}, err
	}
	s.UpdatedAt = nil
	switchedOn := map[string]time.Time{}
	for k, v := range before.SwitchedOn {
		switchedOn[k] = v
	}
	for _, key := range UpgradeSwitches(before, s) {
		switchedOn[key] = now
	}
	s.SwitchedOn = switchedOn
	raw, err := json.Marshal(s)
	if err != nil {
		return UpgradeSettings{}, err
	}
	_, err = r.pool.Exec(ctx, `INSERT INTO upgrade_settings(server_id,settings,updated_by,updated_at) VALUES($1,$2,$3,$4)
ON CONFLICT (server_id) DO UPDATE SET settings=EXCLUDED.settings,updated_by=EXCLUDED.updated_by,updated_at=EXCLUDED.updated_at`, serverID, raw, by, now)
	return before, err
}

// UpgradeServer is a server of the guild with its installation and switches.
type UpgradeServer struct {
	ServerID       int64
	InstallationID int64
	OrganizationID int64
	Settings       UpgradeSettings
}

// GuildServers lists the guild's servers that have an installation, with their switches.
func (r *UpgradeRepository) GuildServers(ctx context.Context, guildID int64) ([]UpgradeServer, error) {
	rows, err := r.pool.Query(ctx, `SELECT DISTINCT ON (gs.id) gs.id,i.id,i.organization_id,COALESCE(u.settings,'{}'::jsonb)
FROM game_servers gs JOIN installations i ON i.game_server_id=gs.id
LEFT JOIN upgrade_settings u ON u.server_id=gs.id
WHERE gs.guild_id=$1 ORDER BY gs.id, i.id DESC`, guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpgradeServer
	for rows.Next() {
		var s UpgradeServer
		var raw []byte
		if err := rows.Scan(&s.ServerID, &s.InstallationID, &s.OrganizationID, &raw); err != nil {
			return nil, err
		}
		s.Settings = DefaultUpgradeSettings()
		if err := json.Unmarshal(raw, &s.Settings); err != nil {
			s.Settings = DefaultUpgradeSettings()
		}
		if s.Settings.WinbackDays == 0 {
			s.Settings.WinbackDays = 7
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// --- 1. priority queue rewards ---------------------------------------------------------------------

// PriorityGrant is a name Champion put on a server's priority list automatically.
type PriorityGrant struct {
	Name     string
	Reason   string
	PlayerID int64
}

// PriorityGrants lists the names Champion added to the server's list automatically.
func (r *UpgradeRepository) PriorityGrants(ctx context.Context, serverID int64) ([]PriorityGrant, error) {
	rows, err := r.pool.Query(ctx, `SELECT name,reason,player_id FROM priority_auto_grants WHERE server_id=$1 ORDER BY granted_at`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PriorityGrant{}
	for rows.Next() {
		var g PriorityGrant
		if err := rows.Scan(&g.Name, &g.Reason, &g.PlayerID); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RecordPriorityGrants stores the names added and forgets the names removed, in one go.
func (r *UpgradeRepository) RecordPriorityGrants(ctx context.Context, serverID int64, added []PriorityGrant, removed []string, now time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, g := range added {
		if _, err := tx.Exec(ctx, `INSERT INTO priority_auto_grants(server_id,name_key,name,reason,player_id,granted_at) VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT (server_id,name_key) DO UPDATE SET reason=EXCLUDED.reason,player_id=EXCLUDED.player_id`, serverID, strings.ToLower(g.Name), g.Name, g.Reason, g.PlayerID, now); err != nil {
			return err
		}
	}
	for _, name := range removed {
		if _, err := tx.Exec(ctx, `DELETE FROM priority_auto_grants WHERE server_id=$1 AND name_key=$2`, serverID, strings.ToLower(name)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ActiveSupporters lists the guild's players holding an active supporter tier.
func (r *UpgradeRepository) ActiveSupporters(ctx context.Context, guildID int64, now time.Time) ([]PriorityGrant, error) {
	rows, err := r.pool.Query(ctx, `SELECT DISTINCT p.display_name,p.id FROM vip_members m JOIN players p ON p.id=m.player_id
WHERE m.guild_id=$1 AND m.revoked_at IS NULL AND (m.expires_at IS NULL OR m.expires_at>$2) AND p.display_name<>''`, guildID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PriorityGrant{}
	for rows.Next() {
		g := PriorityGrant{Reason: "SUPPORTER"}
		if err := rows.Scan(&g.Name, &g.PlayerID); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// PerkHoldsPriority reports whether an active perk store purchase pays for the player's priority.
func (r *UpgradeRepository) PerkHoldsPriority(ctx context.Context, guildID, playerID int64) (bool, error) {
	var yes bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM perk_purchases WHERE guild_id=$1 AND recipient_player_id=$2 AND status='ACTIVE' AND priority_queue)`, guildID, playerID).Scan(&yes)
	return yes, err
}

// --- shared: notices and Discord links -------------------------------------------------------------

// ClaimNotice records that a notice of kind about ref is going to the player now. It returns false
// when it was already sent, so every automation's message goes out at most once.
func (r *UpgradeRepository) ClaimNotice(ctx context.Context, kind string, serverID, playerID int64, ref string, now time.Time) (bool, error) {
	tag, err := r.pool.Exec(ctx, `INSERT INTO upgrade_notices(kind,server_id,player_id,ref,sent_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, kind, serverID, playerID, ref, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// NoticeSentSince reports whether a notice of kind went to the player after since.
func (r *UpgradeRepository) NoticeSentSince(ctx context.Context, kind string, serverID, playerID int64, since time.Time) (bool, error) {
	var yes bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM upgrade_notices WHERE kind=$1 AND server_id=$2 AND player_id=$3 AND sent_at>$4)`, kind, serverID, playerID, since).Scan(&yes)
	return yes, err
}

// NoticeExists reports whether a notice was recorded.
func (r *UpgradeRepository) NoticeExists(ctx context.Context, kind string, serverID, playerID int64, ref string) (bool, error) {
	var yes bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM upgrade_notices WHERE kind=$1 AND server_id=$2 AND player_id=$3 AND ref=$4)`, kind, serverID, playerID, ref).Scan(&yes)
	return yes, err
}

// DiscordUser is the player's verified Discord account, "" when they have none.
func (r *UpgradeRepository) DiscordUser(ctx context.Context, guildID, playerID int64) (string, error) {
	var id string
	err := r.pool.QueryRow(ctx, `SELECT discord_user_id FROM player_links WHERE guild_id=$1 AND player_id=$2 AND status='VERIFIED' ORDER BY id DESC LIMIT 1`, guildID, playerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// --- 4. season-end rewards -------------------------------------------------------------------------

// EndedSeason is a ranked season that ended recently.
type EndedSeason struct {
	ID       int64
	ServerID int64
	StartsAt time.Time
	EndsAt   time.Time
}

// RecentlyEndedSeasons lists the server's ranked seasons that ended after since.
func (r *UpgradeRepository) RecentlyEndedSeasons(ctx context.Context, serverID int64, since time.Time) ([]EndedSeason, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,server_id,starts_at,ends_at FROM ranked_seasons
WHERE scope='SERVER' AND status='ARCHIVED' AND server_id=$1 AND ends_at IS NOT NULL AND ends_at>$2 ORDER BY ends_at`, serverID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EndedSeason
	for rows.Next() {
		var s EndedSeason
		if err := rows.Scan(&s.ID, &s.ServerID, &s.StartsAt, &s.EndsAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SeasonFinisher is a player's final standing in a season.
type SeasonFinisher struct {
	PlayerID int64
	Name     string
	RP       int64
	Kills    int
}

// SeasonTop ranks a season's players by RP (ties: earlier player id first, as the live board does).
func (r *UpgradeRepository) SeasonTop(ctx context.Context, seasonID int64, limit int) ([]SeasonFinisher, error) {
	rows, err := r.pool.Query(ctx, `SELECT a.attacker_key::BIGINT,COALESCE(p.display_name,''),SUM(a.amount)::BIGINT,COUNT(*)::INT
FROM ranked_awards a LEFT JOIN players p ON p.id=a.attacker_key::BIGINT
WHERE a.season_id=$1 AND a.outcome='AWARDED' GROUP BY a.attacker_key,p.display_name
ORDER BY SUM(a.amount) DESC, a.attacker_key::BIGINT ASC LIMIT $2`, seasonID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeasonFinisher
	for rows.Next() {
		var f SeasonFinisher
		if err := rows.Scan(&f.PlayerID, &f.Name, &f.RP, &f.Kills); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// --- 5. live event scoreboard ----------------------------------------------------------------------

// FactionNames maps faction ids to "[TAG] Name".
func (r *UpgradeRepository) FactionNames(ctx context.Context, guildID int64, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT id,'['||tag||'] '||name FROM factions WHERE guild_id=$1 AND id=ANY($2)`, guildID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// RecentlyEndedEvents lists the guild's events that ended after since.
func (r *UpgradeRepository) RecentlyEndedEvents(ctx context.Context, guildID int64, since time.Time) ([]CompetitiveEvent, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,guild_id,COALESCE(season_id,0),event_type,name,COALESCE(description,''),status,starts_at,ends_at,config
FROM competitive_events WHERE guild_id=$1 AND status IN ('ENDED','FINALIZED') AND ends_at>$2 AND event_type<>'HOT_ZONE' ORDER BY ends_at`, guildID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CompetitiveEvent
	for rows.Next() {
		var e CompetitiveEvent
		if err := rows.Scan(&e.ID, &e.GuildID, &e.SeasonID, &e.Type, &e.Name, &e.Description, &e.Status, &e.StartsAt, &e.EndsAt, &e.Config); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- 11. rivals and weapon mastery -----------------------------------------------------------------

// Rival is another player and how many times a kill went one way between them.
type Rival struct {
	PlayerID int64  `json:"playerId"`
	Name     string `json:"name"`
	Kills    int    `json:"kills"`
}

// WeaponRecord is a player's kills with one weapon on a server.
type WeaponRecord struct {
	Weapon    string   `json:"weapon"`
	Kills     int      `json:"kills"`
	Headshots int      `json:"headshots"`
	LongestM  *float64 `json:"longestM"`
}

// Rivals returns who killed the player most (nemesis) and whom the player killed most, on the server.
func (r *UpgradeRepository) Rivals(ctx context.Context, guildID, serverID, playerID int64) (nemesis, victim *Rival, err error) {
	read := func(by, of string) (*Rival, error) {
		var rv Rival
		err := r.pool.QueryRow(ctx, `SELECT k.`+by+`,COALESCE(p.display_name,''),COUNT(*)::INT FROM kills k
LEFT JOIN players p ON p.id=k.`+by+`
WHERE k.guild_id=$1 AND k.server_id=$2 AND k.`+of+`=$3 AND k.`+by+` IS NOT NULL AND k.`+by+`<>$3
GROUP BY k.`+by+`,p.display_name ORDER BY COUNT(*) DESC, MAX(k.id) DESC LIMIT 1`, guildID, serverID, playerID).Scan(&rv.PlayerID, &rv.Name, &rv.Kills)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &rv, nil
	}
	if nemesis, err = read("killer_player_id", "victim_player_id"); err != nil {
		return nil, nil, err
	}
	victim, err = read("victim_player_id", "killer_player_id")
	return nemesis, victim, err
}

// WeaponRecords lists the player's kills by weapon on the server, most used first.
func (r *UpgradeRepository) WeaponRecords(ctx context.Context, guildID, serverID, playerID int64, limit int) ([]WeaponRecord, error) {
	rows, err := r.pool.Query(ctx, `SELECT COALESCE(NULLIF(k.weapon_display,''),NULLIF(k.weapon_raw,''),'Unknown') AS w,COUNT(*)::INT,
COUNT(*) FILTER (WHERE k.headshot)::INT,MAX(k.distance)
FROM kills k WHERE k.guild_id=$1 AND k.server_id=$2 AND k.killer_player_id=$3 AND k.victim_player_id IS NOT NULL AND k.victim_player_id<>$3
GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT $4`, guildID, serverID, playerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WeaponRecord{}
	for rows.Next() {
		var w WeaponRecord
		if err := rows.Scan(&w.Weapon, &w.Kills, &w.Headshots, &w.LongestM); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// --- 13. leaderboard movement ----------------------------------------------------------------------

// PreviousPositions is the board's most recent recorded positions before day (nil when none).
func (r *UpgradeRepository) PreviousPositions(ctx context.Context, guildID int64, board string, day time.Time) (map[string]int, error) {
	var raw []byte
	err := r.pool.QueryRow(ctx, `SELECT positions FROM leaderboard_positions WHERE guild_id=$1 AND board=$2 AND day<$3::DATE ORDER BY day DESC LIMIT 1`,
		guildID, board, day.Format("2006-01-02")).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	return out, json.Unmarshal(raw, &out)
}

// RecordPositions stores the board's positions for day; the first record of a day is kept. Records
// older than two weeks are dropped.
func (r *UpgradeRepository) RecordPositions(ctx context.Context, guildID int64, board string, day time.Time, positions map[string]int) error {
	raw, err := json.Marshal(positions)
	if err != nil {
		return err
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO leaderboard_positions(guild_id,board,day,positions) VALUES($1,$2,$3::DATE,$4) ON CONFLICT DO NOTHING`,
		guildID, board, day.Format("2006-01-02"), raw); err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `DELETE FROM leaderboard_positions WHERE guild_id=$1 AND board=$2 AND day<$3::DATE-14`, guildID, board, day.Format("2006-01-02"))
	return err
}

// --- 19. rent reminders ----------------------------------------------------------------------------

// RentDue is a rented base whose rent falls due in a window, with who should hear about it.
type RentDue struct {
	BaseID      int64
	BaseName    string
	DueAt       time.Time
	PricePoints int64
	PeriodDays  int
	GuildID     int64
	OwnerID     int64
	// FactionMates are the owner's active faction mates (not the owner).
	FactionMates []int64
}

// RentDueBetween lists the server's rented bases (player-requested and approved) whose rent is due
// after from and no later than to.
func (r *UpgradeRepository) RentDueBetween(ctx context.Context, serverID int64, from, to time.Time) ([]RentDue, error) {
	rows, err := r.pool.Query(ctx, `SELECT b.id,b.name,base_rent_due_at(b.id),rs.price_points,rs.period_days,b.guild_id,b.owner_player_id,
COALESCE((SELECT array_agg(m2.player_id) FROM faction_members m1 JOIN faction_members m2 ON m2.faction_id=m1.faction_id AND m2.active AND m2.player_id<>b.owner_player_id
  WHERE m1.guild_id=b.guild_id AND m1.player_id=b.owner_player_id AND m1.active),'{}')
FROM case_registered_bases b
JOIN base_rent_settings rs ON rs.installation_id=b.installation_id AND rs.server_id=b.server_id AND rs.enabled
WHERE b.server_id=$1 AND b.state<>'REVOKED' AND EXISTS (SELECT 1 FROM case_base_requests rq WHERE rq.base_id=b.id AND rq.status='APPROVED')
  AND base_rent_due_at(b.id)>$2 AND base_rent_due_at(b.id)<=$3
ORDER BY 3 LIMIT 200`, serverID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RentDue
	for rows.Next() {
		var d RentDue
		if err := rows.Scan(&d.BaseID, &d.BaseName, &d.DueAt, &d.PricePoints, &d.PeriodDays, &d.GuildID, &d.OwnerID, &d.FactionMates); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// --- 21. daily play reward -------------------------------------------------------------------------

// PlayDays lists, for players seen on the server on `day`, the days (newest first, `day` included)
// they were seen during the week up to it. Backfilled days do not count.
func (r *UpgradeRepository) PlayDays(ctx context.Context, serverID int64, day time.Time) (map[int64][]time.Time, error) {
	d := day.Format("2006-01-02")
	rows, err := r.pool.Query(ctx, `SELECT a.player_id,a.day::TIMESTAMP FROM player_daily_activity a
WHERE a.server_id=$1 AND a.source='OBSERVED' AND a.sessions>0 AND a.day>$2::DATE-7 AND a.day<=$2::DATE
  AND EXISTS (SELECT 1 FROM player_daily_activity t WHERE t.server_id=a.server_id AND t.player_id=a.player_id AND t.day=$2::DATE AND t.source='OBSERVED' AND t.sessions>0)
ORDER BY a.player_id, a.day DESC`, serverID, d)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]time.Time{}
	for rows.Next() {
		var p int64
		var t time.Time
		if err := rows.Scan(&p, &t); err != nil {
			return nil, err
		}
		out[p] = append(out[p], t)
	}
	return out, rows.Err()
}

// --- 23. shop order updates ------------------------------------------------------------------------

// OrderStep is a recent shop order with the step its delivery reached.
type OrderStep struct {
	PurchaseID  int64
	PlayerID    int64
	Items       string
	TotalPoints int64
	Status      string
	// AttemptState is the latest automatic delivery attempt's state ("" for manual delivery).
	AttemptState string
}

// RecentOrders lists the server's shop orders from the last two days with their latest delivery step.
func (r *UpgradeRepository) RecentOrders(ctx context.Context, serverID int64, since time.Time) ([]OrderStep, error) {
	rows, err := r.pool.Query(ctx, `SELECT p.id,p.player_id,
COALESCE((SELECT string_agg(i.quantity||'× '||i.product_name, ', ' ORDER BY i.id) FROM shop_purchase_items i WHERE i.purchase_id=p.id),''),
p.total_points,p.status,
COALESCE((SELECT a.state FROM shop_deliveries d JOIN shop_delivery_attempts a ON a.delivery_id=d.id
  WHERE d.purchase_id=p.id ORDER BY a.id DESC LIMIT 1),'')
FROM shop_purchases p WHERE p.game_server_id=$1 AND p.created_at>$2 ORDER BY p.id LIMIT 300`, serverID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrderStep
	for rows.Next() {
		var o OrderStep
		if err := rows.Scan(&o.PurchaseID, &o.PlayerID, &o.Items, &o.TotalPoints, &o.Status, &o.AttemptState); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// --- 24. renewal reminders and gift notes ----------------------------------------------------------

// Expiring is a perk purchase or supporter tier that ends (or renews) soon.
type Expiring struct {
	Kind      string // PERK or TIER
	ID        int64
	PlayerID  int64
	Name      string // offer or tier name
	ExpiresAt time.Time
	// Renews is true for a monthly perk set to renew (it is charged, not ended).
	Renews      bool
	PricePoints int64
}

// ExpiringSoon lists the guild's active perk purchases and supporter tiers ending between from and
// to. A tier that a perk purchase pays for is left to the purchase's reminder.
func (r *UpgradeRepository) ExpiringSoon(ctx context.Context, guildID int64, from, to time.Time) ([]Expiring, error) {
	rows, err := r.pool.Query(ctx, `SELECT 'PERK',p.id,p.recipient_player_id,o.name,p.expires_at,(p.billing='MONTHLY' AND p.auto_renew),p.price_points
FROM perk_purchases p JOIN perk_offers o ON o.id=p.offer_id
WHERE p.guild_id=$1 AND p.status='ACTIVE' AND p.expires_at>$2 AND p.expires_at<=$3
UNION ALL
SELECT 'TIER',m.id,m.player_id,t.name,m.expires_at,FALSE,0
FROM vip_members m JOIN vip_tiers t ON t.id=m.tier_id
WHERE m.guild_id=$1 AND m.revoked_at IS NULL AND m.expires_at>$2 AND m.expires_at<=$3
  AND NOT EXISTS (SELECT 1 FROM perk_purchases p WHERE p.vip_member_id=m.id AND p.status='ACTIVE')
ORDER BY 5 LIMIT 200`, guildID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Expiring
	for rows.Next() {
		var e Expiring
		if err := rows.Scan(&e.Kind, &e.ID, &e.PlayerID, &e.Name, &e.ExpiresAt, &e.Renews, &e.PricePoints); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SaveGiftMessage stores the note written with a gifted purchase (first one wins).
func (r *UpgradeRepository) SaveGiftMessage(ctx context.Context, purchaseID int64, message string) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO perk_gift_messages(purchase_id,message) VALUES($1,$2) ON CONFLICT DO NOTHING`, purchaseID, message)
	return err
}

// GiftMessages returns the notes of the given purchases.
func (r *UpgradeRepository) GiftMessages(ctx context.Context, purchaseIDs []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(purchaseIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT purchase_id,message FROM perk_gift_messages WHERE purchase_id=ANY($1)`, purchaseIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var m string
		if err := rows.Scan(&id, &m); err != nil {
			return nil, err
		}
		out[id] = m
	}
	return out, rows.Err()
}

// --- 26. security digest and appeals ---------------------------------------------------------------

// SecurityWeek is one server's security numbers for a week.
type SecurityWeek struct {
	CasesOpened, CasesClosed, CasesPending int
	StaffAlerts, ShadowVerdicts            int
	RaidAlarms, PerimeterAlerts            int
	ZoneIntrusions, AppealsOpened          int
}

// SecurityWeekCounts counts the server's security activity between from and to.
func (r *UpgradeRepository) SecurityWeekCounts(ctx context.Context, serverID int64, from, to time.Time) (SecurityWeek, error) {
	var w SecurityWeek
	err := r.pool.QueryRow(ctx, `SELECT
 (SELECT COUNT(*) FROM case_review_cases WHERE server_id=$1 AND created_at>=$2 AND created_at<$3)::INT,
 (SELECT COUNT(*) FROM case_review_cases WHERE server_id=$1 AND status<>'PENDING_REVIEW' AND updated_at>=$2 AND updated_at<$3)::INT,
 (SELECT COUNT(*) FROM case_review_cases WHERE server_id=$1 AND status='PENDING_REVIEW')::INT,
 (SELECT COUNT(*) FROM case_alert_deliveries WHERE server_id=$1 AND created_at>=$2 AND created_at<$3)::INT,
 (SELECT COUNT(*) FROM case_shadow_verdicts WHERE server_id=$1 AND created_at>=$2 AND created_at<$3)::INT,
 (SELECT COUNT(*) FROM base_raid_alerts WHERE server_id=$1 AND created_at>=$2 AND created_at<$3)::INT,
 (SELECT COUNT(*) FROM perimeter_watch_alerts WHERE server_id=$1 AND created_at>=$2 AND created_at<$3)::INT,
 (SELECT COUNT(*) FROM zone_intrusions WHERE server_id=$1 AND entered_at>=$2 AND entered_at<$3)::INT,
 (SELECT COUNT(*) FROM player_appeals WHERE server_id=$1 AND created_at>=$2 AND created_at<$3)::INT`, serverID, from, to).Scan(
		&w.CasesOpened, &w.CasesClosed, &w.CasesPending, &w.StaffAlerts, &w.ShadowVerdicts, &w.RaidAlarms, &w.PerimeterAlerts, &w.ZoneIntrusions, &w.AppealsOpened)
	return w, err
}

var (
	ErrAppealOpen     = errors.New("you already have an open appeal; staff will answer it first")
	ErrAppealNotFound = errors.New("appeal not found or already answered")
)

// Appeal is a player's appeal to staff.
type Appeal struct {
	ID         int64      `json:"id"`
	PlayerID   int64      `json:"playerId"`
	PlayerName string     `json:"playerName"`
	Topic      string     `json:"topic"`
	Message    string     `json:"message"`
	Status     string     `json:"status"`
	StaffNote  string     `json:"staffNote"`
	CreatedAt  time.Time  `json:"createdAt"`
	DecidedAt  *time.Time `json:"decidedAt"`
	GuildID    int64      `json:"-"`
	ServerID   int64      `json:"-"`
}

const appealCols = `a.id,a.player_id,COALESCE(p.display_name,''),a.topic,a.message,a.status,a.staff_note,a.created_at,a.decided_at,a.guild_id,a.server_id`

func scanAppeals(rows pgx.Rows, err error) ([]Appeal, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Appeal{}
	for rows.Next() {
		var a Appeal
		if err := rows.Scan(&a.ID, &a.PlayerID, &a.PlayerName, &a.Topic, &a.Message, &a.Status, &a.StaffNote, &a.CreatedAt, &a.DecidedAt, &a.GuildID, &a.ServerID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CreateAppeal records a player's appeal; a player has at most one open appeal per server.
func (r *UpgradeRepository) CreateAppeal(ctx context.Context, guildID, serverID, playerID int64, topic, message string) (Appeal, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `INSERT INTO player_appeals(guild_id,server_id,player_id,topic,message) VALUES($1,$2,$3,$4,$5)
ON CONFLICT (server_id,player_id) WHERE status='OPEN' DO NOTHING RETURNING id`, guildID, serverID, playerID, topic, message).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Appeal{}, ErrAppealOpen
	}
	if err != nil {
		return Appeal{}, err
	}
	list, err := scanAppeals(r.pool.Query(ctx, `SELECT `+appealCols+` FROM player_appeals a LEFT JOIN players p ON p.id=a.player_id WHERE a.id=$1`, id))
	if err != nil || len(list) == 0 {
		return Appeal{}, err
	}
	return list[0], nil
}

// PlayerAppeals lists a player's appeals on the server, newest first.
func (r *UpgradeRepository) PlayerAppeals(ctx context.Context, serverID, playerID int64) ([]Appeal, error) {
	return scanAppeals(r.pool.Query(ctx, `SELECT `+appealCols+` FROM player_appeals a LEFT JOIN players p ON p.id=a.player_id
WHERE a.server_id=$1 AND a.player_id=$2 ORDER BY a.id DESC LIMIT 20`, serverID, playerID))
}

// ServerAppeals lists the server's appeals: open ones first, then the latest answered.
func (r *UpgradeRepository) ServerAppeals(ctx context.Context, serverID int64) ([]Appeal, error) {
	return scanAppeals(r.pool.Query(ctx, `SELECT `+appealCols+` FROM player_appeals a LEFT JOIN players p ON p.id=a.player_id
WHERE a.server_id=$1 ORDER BY (a.status='OPEN') DESC, a.id DESC LIMIT 100`, serverID))
}

// DecideAppeal answers an open appeal.
func (r *UpgradeRepository) DecideAppeal(ctx context.Context, serverID, appealID int64, accept bool, note, by string, now time.Time) (Appeal, error) {
	status := "REJECTED"
	if accept {
		status = "ACCEPTED"
	}
	tag, err := r.pool.Exec(ctx, `UPDATE player_appeals SET status=$3,staff_note=$4,decided_by=$5,decided_at=$6 WHERE server_id=$1 AND id=$2 AND status='OPEN'`,
		serverID, appealID, status, note, by, now)
	if err != nil {
		return Appeal{}, err
	}
	if tag.RowsAffected() == 0 {
		return Appeal{}, ErrAppealNotFound
	}
	list, err := scanAppeals(r.pool.Query(ctx, `SELECT `+appealCols+` FROM player_appeals a LEFT JOIN players p ON p.id=a.player_id WHERE a.id=$1`, appealID))
	if err != nil || len(list) == 0 {
		return Appeal{}, err
	}
	return list[0], nil
}

// --- 27. zone alert player -------------------------------------------------------------------------

// ZoneAlertPlayer is the player a zone's alerts also go to by DM.
type ZoneAlertPlayer struct {
	PlayerID int64  `json:"playerId"`
	Name     string `json:"name"`
}

// ZoneAlertPlayer returns the zone's alert player, nil when none.
func (r *UpgradeRepository) ZoneAlertPlayer(ctx context.Context, installationID, zoneID int64) (*ZoneAlertPlayer, error) {
	var z ZoneAlertPlayer
	err := r.pool.QueryRow(ctx, `SELECT a.player_id,COALESCE(p.display_name,'') FROM zone_alert_players a LEFT JOIN players p ON p.id=a.player_id
WHERE a.installation_id=$1 AND a.zone_id=$2`, installationID, zoneID).Scan(&z.PlayerID, &z.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &z, nil
}

// SetZoneAlertPlayer sets (playerID > 0) or clears (0) the zone's alert player.
func (r *UpgradeRepository) SetZoneAlertPlayer(ctx context.Context, installationID, zoneID, playerID int64, by string) error {
	if playerID <= 0 {
		_, err := r.pool.Exec(ctx, `DELETE FROM zone_alert_players WHERE installation_id=$1 AND zone_id=$2`, installationID, zoneID)
		return err
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO zone_alert_players(zone_id,installation_id,player_id,set_by) VALUES($2,$1,$3,$4)
ON CONFLICT (zone_id) DO UPDATE SET player_id=EXCLUDED.player_id,set_by=EXCLUDED.set_by,updated_at=NOW()`, installationID, zoneID, playerID, by)
	return err
}

// FindPlayerByName finds the guild's player with exactly this name (ignoring case); 0 when none.
func (r *UpgradeRepository) FindPlayerByName(ctx context.Context, guildID int64, name string) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT id FROM players WHERE guild_id=$1 AND LOWER(display_name)=LOWER($2) ORDER BY id DESC LIMIT 1`, guildID, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// --- 28. server of the week ------------------------------------------------------------------------

// Spotlight is the network's server of the week.
type Spotlight struct {
	WeekStart      time.Time `json:"weekStart"`
	InstallationID int64     `json:"installationId"`
	ServerID       int64     `json:"-"`
	GuildID        int64     `json:"-"`
	Name           string    `json:"name"`
	Platform       string    `json:"platform"`
	ActivePlayers  int       `json:"activePlayers7d"`
	Kills          int       `json:"kills7d"`
}

// SpotlightFor returns the latest pick of a week starting no later than week, nil when none.
func (r *UpgradeRepository) SpotlightFor(ctx context.Context, week time.Time) (*Spotlight, error) {
	var s Spotlight
	err := r.pool.QueryRow(ctx, `SELECT week_start::TIMESTAMP,installation_id,server_id,guild_id,name,platform,active_players,kills FROM network_spotlight WHERE week_start<=$1::DATE ORDER BY week_start DESC LIMIT 1`,
		week.Format("2006-01-02")).Scan(&s.WeekStart, &s.InstallationID, &s.ServerID, &s.GuildID, &s.Name, &s.Platform, &s.ActivePlayers, &s.Kills)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// SaveSpotlight stores the week's pick; false when the week already has one.
func (r *UpgradeRepository) SaveSpotlight(ctx context.Context, s Spotlight) (bool, error) {
	tag, err := r.pool.Exec(ctx, `INSERT INTO network_spotlight(week_start,installation_id,server_id,guild_id,name,platform,active_players,kills) VALUES($1::DATE,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`,
		s.WeekStart.Format("2006-01-02"), s.InstallationID, s.ServerID, s.GuildID, s.Name, s.Platform, s.ActivePlayers, s.Kills)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// --- 29. features channel --------------------------------------------------------------------------

// FeaturesChannel is the guild's features channel (made with /features), "" when it has none.
func (r *UpgradeRepository) FeaturesChannel(ctx context.Context, guildID int64) (string, error) {
	var id string
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(features_channel_id,'') FROM guilds WHERE id=$1`, guildID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}
