package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The live map UAV (docs/LIVE_MAP.md "UAV"): a player buys time with Champion Points and, for that
// time, their live map shows every connected player (their faction mates' maps too when the owner
// shares it). A basic UAV shows a dot per player; a precision UAV adds the player's name, faction,
// last kill weapon, time alive and heading. The points go to the owner's linked character, the way
// the perk store pays.

// Ledger types written only here.
const (
	TxUAVPurchase = "UAV_PURCHASE" // debit: a player bought UAV time
	TxUAVSale     = "UAV_SALE"     // credit: the owner received that payment
)

// UAV tiers.
const (
	UAVBasic     = "BASIC"
	UAVPrecision = "PRECISION"
	// UAVGhost hides its buyer from every other player's UAV (never from staff, their own faction or
	// the public kill pins).
	UAVGhost = "GHOST"
)

var (
	ErrUAVSettingsInvalid = errors.New("UAV settings are out of range")
	ErrUAVClosed          = errors.New("this UAV is not for sale on this server")
	ErrUAVInvalidRequest  = errors.New("pick a UAV and how long to buy")
)

type UAVSettings struct {
	Enabled          bool       `json:"enabled"`
	BlockMinutes     int        `json:"blockMinutes"`
	BasicPrice       int64      `json:"basicPrice"`
	PrecisionEnabled bool       `json:"precisionEnabled"`
	PrecisionPrice   int64      `json:"precisionPrice"`
	GhostEnabled     bool       `json:"ghostEnabled"`
	GhostPrice       int64      `json:"ghostPrice"`
	MaxBlocks        int        `json:"maxBlocks"`
	DelaySeconds     int        `json:"delaySeconds"`
	ShareFaction     bool       `json:"shareFaction"`
	UpdatedAt        *time.Time `json:"updatedAt"`
}

func DefaultUAVSettings() UAVSettings {
	return UAVSettings{BlockMinutes: 15, BasicPrice: 250, PrecisionEnabled: true, PrecisionPrice: 600, GhostPrice: 400, MaxBlocks: 8, ShareFaction: true}
}

func (s UAVSettings) Validate() error {
	if s.BlockMinutes < 5 || s.BlockMinutes > 120 || s.BasicPrice < 1 || s.BasicPrice > 10_000_000 || s.PrecisionPrice < 1 || s.PrecisionPrice > 10_000_000 || s.GhostPrice < 1 || s.GhostPrice > 10_000_000 ||
		s.MaxBlocks < 1 || s.MaxBlocks > 48 || s.DelaySeconds < 0 || s.DelaySeconds > 600 {
		return ErrUAVSettingsInvalid
	}
	return nil
}

// Price is the price of one block of the tier.
func (s UAVSettings) Price(tier string) int64 {
	switch tier {
	case UAVPrecision:
		return s.PrecisionPrice
	case UAVGhost:
		return s.GhostPrice
	}
	return s.BasicPrice
}

type UAVRepository struct{ pool *pgxpool.Pool }

func NewUAVRepository(pool *pgxpool.Pool) *UAVRepository { return &UAVRepository{pool: pool} }

const uavSettingsCols = `enabled,block_minutes,basic_price,precision_enabled,precision_price,ghost_enabled,ghost_price,max_blocks,delay_seconds,share_faction,updated_at`

func loadUAVSettings(ctx context.Context, q querier, serverID int64) (UAVSettings, error) {
	s := DefaultUAVSettings()
	var updated time.Time
	err := q.QueryRow(ctx, `SELECT `+uavSettingsCols+` FROM uav_settings WHERE server_id=$1`, serverID).
		Scan(&s.Enabled, &s.BlockMinutes, &s.BasicPrice, &s.PrecisionEnabled, &s.PrecisionPrice, &s.GhostEnabled, &s.GhostPrice, &s.MaxBlocks, &s.DelaySeconds, &s.ShareFaction, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultUAVSettings(), nil
	}
	if err != nil {
		return s, err
	}
	s.UpdatedAt = &updated
	return s, nil
}

func (r *UAVRepository) Settings(ctx context.Context, serverID int64) (UAVSettings, error) {
	return loadUAVSettings(ctx, r.pool, serverID)
}

func (r *UAVRepository) SaveSettings(ctx context.Context, serverID int64, s UAVSettings, by string, now time.Time) (UAVSettings, error) {
	if err := s.Validate(); err != nil {
		return s, err
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO uav_settings(server_id,enabled,block_minutes,basic_price,precision_enabled,precision_price,ghost_enabled,ghost_price,max_blocks,delay_seconds,share_faction,updated_at,updated_by)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT (server_id) DO UPDATE SET enabled=EXCLUDED.enabled,block_minutes=EXCLUDED.block_minutes,
basic_price=EXCLUDED.basic_price,precision_enabled=EXCLUDED.precision_enabled,precision_price=EXCLUDED.precision_price,ghost_enabled=EXCLUDED.ghost_enabled,
ghost_price=EXCLUDED.ghost_price,max_blocks=EXCLUDED.max_blocks,
delay_seconds=EXCLUDED.delay_seconds,share_faction=EXCLUDED.share_faction,updated_at=EXCLUDED.updated_at,updated_by=EXCLUDED.updated_by`,
		serverID, s.Enabled, s.BlockMinutes, s.BasicPrice, s.PrecisionEnabled, s.PrecisionPrice, s.GhostEnabled, s.GhostPrice, s.MaxBlocks, s.DelaySeconds, s.ShareFaction, now, by)
	if err != nil {
		return s, err
	}
	s.UpdatedAt = &now
	return s, nil
}

// UAVPass is one purchase.
type UAVPass struct {
	ID        int64     `json:"id"`
	PlayerID  int64     `json:"-"`
	BuyerName string    `json:"buyerName"`
	FactionID *int64    `json:"-"`
	Tier      string    `json:"tier"`
	Blocks    int       `json:"blocks"`
	Minutes   int       `json:"minutes"`
	Price     int64     `json:"price"`
	StartsAt  time.Time `json:"startsAt"`
	EndsAt    time.Time `json:"endsAt"`
	CreatedAt time.Time `json:"createdAt"`
}

const uavPassCols = `u.id,u.player_id,COALESCE(p.display_name,''),u.faction_id,u.tier,u.blocks,u.minutes,u.price,u.starts_at,u.ends_at,u.created_at`
const uavPassFrom = ` FROM uav_passes u LEFT JOIN players p ON p.id=u.player_id `

func scanUAVPass(row pgx.Row) (UAVPass, error) {
	var u UAVPass
	err := row.Scan(&u.ID, &u.PlayerID, &u.BuyerName, &u.FactionID, &u.Tier, &u.Blocks, &u.Minutes, &u.Price, &u.StartsAt, &u.EndsAt, &u.CreatedAt)
	return u, err
}

// Buy charges the player blocks × the tier's block price and adds blocks × the block length of UAV
// time of that tier, starting now or when the player's running UAV of the same tier ends. factionID
// is the buyer's hub faction now (0 = none), whose members share the UAV when the owner allows it.
// requestKey makes it idempotent: a replay returns the original pass and charges nothing.
func (r *UAVRepository) Buy(ctx context.Context, scope PerkScope, playerID, factionID int64, tier string, blocks int, requestKey string, now time.Time) (UAVPass, int64, error) {
	if scope.ServerID <= 0 || playerID <= 0 || blocks < 1 || len(requestKey) < 8 || len(requestKey) > 80 || (tier != UAVBasic && tier != UAVPrecision && tier != UAVGhost) {
		return UAVPass{}, 0, ErrUAVInvalidRequest
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return UAVPass{}, 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// One player's purchases queue one after the other.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('uav:'||$1::BIGINT::TEXT||':'||$2::BIGINT::TEXT,0))`, scope.ServerID, playerID); err != nil {
		return UAVPass{}, 0, err
	}
	if prev, err := scanUAVPass(tx.QueryRow(ctx, `SELECT `+uavPassCols+uavPassFrom+`WHERE u.server_id=$1 AND u.player_id=$2 AND u.request_key=$3`,
		scope.ServerID, playerID, requestKey)); err == nil {
		var balance int64
		_ = tx.QueryRow(ctx, `SELECT COALESCE((SELECT balance FROM player_points WHERE guild_id=$1 AND player_id=$2),0)`, scope.GuildID, playerID).Scan(&balance)
		return prev, balance, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return UAVPass{}, 0, err
	}
	s, err := loadUAVSettings(ctx, tx, scope.ServerID)
	if err != nil {
		return UAVPass{}, 0, err
	}
	// Ghost is only sold while the UAV is: with no UAV there is nothing to hide from.
	if !s.Enabled || (tier == UAVPrecision && !s.PrecisionEnabled) || (tier == UAVGhost && !s.GhostEnabled) {
		return UAVPass{}, 0, ErrUAVClosed
	}
	if blocks > s.MaxBlocks {
		return UAVPass{}, 0, ErrUAVInvalidRequest
	}
	start := now
	var runningEnd *time.Time
	if err := tx.QueryRow(ctx, `SELECT MAX(ends_at) FROM uav_passes WHERE server_id=$1 AND player_id=$2 AND tier=$3 AND ends_at>$4`,
		scope.ServerID, playerID, tier, now).Scan(&runningEnd); err != nil {
		return UAVPass{}, 0, err
	}
	if runningEnd != nil && runningEnd.After(start) {
		start = *runningEnd
	}
	minutes := blocks * s.BlockMinutes
	price := int64(blocks) * s.Price(tier)
	label := map[string]string{UAVBasic: "UAV", UAVPrecision: "Precision UAV", UAVGhost: "Ghost"}[tier]
	desc := fmt.Sprintf("Live map %s · %d minutes", label, minutes)
	ref := "uav:" + requestKey
	entry, err := applyLedger(ctx, tx, LedgerParams{GuildID: scope.GuildID, PlayerID: playerID, ServerID: scope.ServerID, Type: TxUAVPurchase,
		Amount: price, CreatedBy: "SYSTEM", ReferenceID: ref, Description: desc}, true)
	if err != nil {
		return UAVPass{}, 0, err
	}
	var owner *int64
	if id, ok, err := ownerPlayer(ctx, tx, scope); err != nil {
		return UAVPass{}, 0, err
	} else if ok {
		if _, err := applyLedger(ctx, tx, LedgerParams{GuildID: scope.GuildID, PlayerID: id, ServerID: scope.ServerID, Type: TxUAVSale,
			Amount: price, CreatedBy: "SYSTEM", ReferenceID: fmt.Sprintf("%s:%d", ref, playerID), Description: desc}, false); err != nil {
			return UAVPass{}, 0, err
		}
		owner = &id
		if id == playerID {
			if err := tx.QueryRow(ctx, `SELECT balance FROM player_points WHERE guild_id=$1 AND player_id=$2`, scope.GuildID, playerID).Scan(&entry.BalanceAfter); err != nil {
				return UAVPass{}, 0, err
			}
		}
	}
	var faction *int64
	// A ghost is the buyer's own: never shared.
	if factionID > 0 && tier != UAVGhost {
		faction = &factionID
	}
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO uav_passes(guild_id,server_id,installation_id,player_id,faction_id,tier,blocks,minutes,price,owner_player_id,request_key,starts_at,ends_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`, scope.GuildID, scope.ServerID, scope.InstallationID, playerID, faction, tier, blocks, minutes, price, owner,
		requestKey, start, start.Add(time.Duration(minutes)*time.Minute)).Scan(&id); err != nil {
		return UAVPass{}, 0, err
	}
	pass, err := scanUAVPass(tx.QueryRow(ctx, `SELECT `+uavPassCols+uavPassFrom+`WHERE u.id=$1`, id))
	if err != nil {
		return UAVPass{}, 0, err
	}
	return pass, entry.BalanceAfter, tx.Commit(ctx)
}

// ActivePass returns the UAV covering the player now: their own, else (when shared) one bought by a
// member of their current faction. A precision UAV wins over a basic one, then the one ending last;
// nil when none. EndsAt is extended over more time of the same tier bought back to back.
func (r *UAVRepository) ActivePass(ctx context.Context, serverID, playerID, factionID int64, share bool, now time.Time) (*UAVPass, error) {
	pass, err := scanUAVPass(r.pool.QueryRow(ctx, `SELECT `+uavPassCols+uavPassFrom+`
WHERE u.server_id=$1 AND u.tier IN ('BASIC','PRECISION') AND u.starts_at<=$4 AND u.ends_at>$4 AND (u.player_id=$2 OR ($5 AND $3::BIGINT>0 AND u.faction_id=$3))
ORDER BY (u.tier='PRECISION') DESC, u.ends_at DESC, u.id DESC LIMIT 1`, serverID, playerID, factionID, now, share))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var last time.Time
	if err := r.pool.QueryRow(ctx, `SELECT MAX(ends_at) FROM uav_passes WHERE server_id=$1 AND player_id=$2 AND tier=$3`, serverID, pass.PlayerID, pass.Tier).
		Scan(&last); err == nil && last.After(pass.EndsAt) {
		pass.EndsAt = last
	}
	return &pass, nil
}

// ActiveGhost returns the player's running ghost (its end extended over more ghost time bought back
// to back), nil when none.
func (r *UAVRepository) ActiveGhost(ctx context.Context, serverID, playerID int64, now time.Time) (*UAVPass, error) {
	pass, err := scanUAVPass(r.pool.QueryRow(ctx, `SELECT `+uavPassCols+uavPassFrom+`
WHERE u.server_id=$1 AND u.player_id=$2 AND u.tier='GHOST' AND u.starts_at<=$3 AND u.ends_at>$3 ORDER BY u.ends_at DESC LIMIT 1`, serverID, playerID, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var last time.Time
	if err := r.pool.QueryRow(ctx, `SELECT MAX(ends_at) FROM uav_passes WHERE server_id=$1 AND player_id=$2 AND tier='GHOST'`, serverID, playerID).Scan(&last); err == nil && last.After(pass.EndsAt) {
		pass.EndsAt = last
	}
	return &pass, nil
}

// Ghosted returns the players of the server hidden by a running ghost.
func (r *UAVRepository) Ghosted(ctx context.Context, serverID int64, now time.Time) (map[int64]bool, error) {
	rows, err := r.pool.Query(ctx, `SELECT DISTINCT player_id FROM uav_passes WHERE server_id=$1 AND tier='GHOST' AND starts_at<=$2 AND ends_at>$2`, serverID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// UAVSales summarises the server's UAV sales for the owner.
type UAVSales struct {
	Passes  int       `json:"passes"`
	Points  int64     `json:"points"`
	Players int       `json:"players"`
	Recent  []UAVPass `json:"recent"`
}

func (r *UAVRepository) Sales(ctx context.Context, serverID int64, since time.Time, limit int) (UAVSales, error) {
	out := UAVSales{Recent: []UAVPass{}}
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*)::INT,COALESCE(SUM(price),0)::BIGINT,COUNT(DISTINCT player_id)::INT FROM uav_passes WHERE server_id=$1 AND created_at>=$2`,
		serverID, since).Scan(&out.Passes, &out.Points, &out.Players); err != nil {
		return out, err
	}
	rows, err := r.pool.Query(ctx, `SELECT `+uavPassCols+uavPassFrom+`WHERE u.server_id=$1 ORDER BY u.created_at DESC LIMIT $2`, serverID, limit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		u, err := scanUAVPass(rows)
		if err != nil {
			return out, err
		}
		out.Recent = append(out.Recent, u)
	}
	return out, rows.Err()
}

// UAVIntel is what a precision UAV adds about one player.
type UAVIntel struct {
	PlayerID     int64
	AliveSeconds *int64
	LifeKills    int
	LastWeapon   string
	LastWeaponAt *time.Time
}

// PrecisionIntel returns, for the server's connected players, their time alive and kills this life
// and the weapon of their latest kill this life (empty when none).
func (r *UAVRepository) PrecisionIntel(ctx context.Context, guildID, serverID int64, now time.Time) (map[int64]UAVIntel, error) {
	rows, err := r.pool.Query(ctx, `SELECT c.player_id,c.playtime_seconds,c.kills,COALESCE(w.weapon,''),w.at FROM (`+currentLifeSQL+` AND a.currently_connected) c
LEFT JOIN LATERAL (SELECT COALESCE(k.weapon_display,k.weapon_raw) AS weapon,COALESCE(k.event_time,k.created_at) AS at FROM kills k
  WHERE k.guild_id=$1 AND k.server_id=$2 AND k.killer_player_id=c.player_id AND k.victim_player_id IS DISTINCT FROM c.player_id
    AND COALESCE(k.event_time,k.created_at)>c.started_at ORDER BY COALESCE(k.event_time,k.created_at) DESC LIMIT 1) w ON TRUE`, guildID, serverID, now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]UAVIntel{}
	for rows.Next() {
		var in UAVIntel
		if err := rows.Scan(&in.PlayerID, &in.AliveSeconds, &in.LifeKills, &in.LastWeapon, &in.LastWeaponAt); err != nil {
			return nil, err
		}
		out[in.PlayerID] = in
	}
	return out, rows.Err()
}
