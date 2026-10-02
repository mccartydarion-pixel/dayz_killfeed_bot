package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/vip"
)

// VIPRepository stores supporter / VIP tiers and memberships.
type VIPRepository struct{ pool *pgxpool.Pool }

func NewVIPRepository(pool *pgxpool.Pool) *VIPRepository { return &VIPRepository{pool: pool} }

var (
	ErrVIPTierNotFound   = errors.New("VIP tier not found")
	ErrVIPTierInUse      = errors.New("VIP tier still has active members")
	ErrVIPTierNameTaken  = errors.New("a tier with that name already exists")
	ErrVIPMemberNotFound = errors.New("VIP membership not found or already ended")
	ErrVIPAlreadyMember  = errors.New("this player already has an active VIP tier; end it first")
	ErrVIPPlayerNotFound = errors.New("player not found")
)

const vipActive = `m.revoked_at IS NULL AND (m.expires_at IS NULL OR m.expires_at > NOW())`

func (r *VIPRepository) ListTiers(ctx context.Context, guildID int64) ([]vip.Tier, error) {
	rows, err := r.pool.Query(ctx, `SELECT t.id,t.name,t.badge,t.color,COALESCE(t.discord_role_id,''),t.reward_multiplier::float8,t.sort_order,
  (SELECT COUNT(*) FROM vip_members m WHERE m.tier_id=t.id AND `+vipActive+`)
FROM vip_tiers t WHERE t.guild_id=$1 ORDER BY t.sort_order,t.id`, guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []vip.Tier{}
	for rows.Next() {
		var t vip.Tier
		if err := rows.Scan(&t.ID, &t.Name, &t.Badge, &t.Color, &t.DiscordRoleID, &t.RewardMultiplier, &t.SortOrder, &t.ActiveMembers); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveTier creates (ID 0) or updates a tier.
func (r *VIPRepository) SaveTier(ctx context.Context, guildID int64, t vip.Tier) (vip.Tier, error) {
	var err error
	if t.ID == 0 {
		err = r.pool.QueryRow(ctx, `INSERT INTO vip_tiers(guild_id,name,badge,color,discord_role_id,reward_multiplier,sort_order) VALUES($1,$2,$3,$4,NULLIF($5,''),$6,$7) RETURNING id`,
			guildID, t.Name, t.Badge, t.Color, t.DiscordRoleID, t.RewardMultiplier, t.SortOrder).Scan(&t.ID)
	} else {
		var id int64
		err = r.pool.QueryRow(ctx, `UPDATE vip_tiers SET name=$3,badge=$4,color=$5,discord_role_id=NULLIF($6,''),reward_multiplier=$7,sort_order=$8,updated_at=NOW() WHERE guild_id=$1 AND id=$2 RETURNING id`,
			guildID, t.ID, t.Name, t.Badge, t.Color, t.DiscordRoleID, t.RewardMultiplier, t.SortOrder).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return t, ErrVIPTierNotFound
		}
	}
	if isUniqueViolation(err) {
		return t, ErrVIPTierNameTaken
	}
	return t, err
}

// DeleteTier removes a tier with no active members (ended memberships go with it).
func (r *VIPRepository) DeleteTier(ctx context.Context, guildID, id int64) error {
	var active int64
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM vip_members m WHERE m.guild_id=$1 AND m.tier_id=$2 AND `+vipActive, guildID, id).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return ErrVIPTierInUse
	}
	tag, err := r.pool.Exec(ctx, `DELETE FROM vip_tiers WHERE guild_id=$1 AND id=$2`, guildID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrVIPTierNotFound
	}
	return nil
}

// VIPMember is one membership with what the Client Hub shows about it.
type VIPMember struct {
	ID            int64      `json:"id"`
	TierID        int64      `json:"tierId"`
	TierName      string     `json:"tierName"`
	PlayerID      int64      `json:"playerId"`
	PlayerName    string     `json:"playerName"`
	DiscordUserID string     `json:"discordUserId,omitempty"`
	RoleID        string     `json:"-"`
	DiscordGuild  string     `json:"-"`
	Note          string     `json:"note,omitempty"`
	GrantedBy     string     `json:"grantedBy,omitempty"`
	GrantedAt     time.Time  `json:"grantedAt"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	RevokedAt     *time.Time `json:"revokedAt,omitempty"`
	RevokedReason string     `json:"revokedReason,omitempty"`
	Active        bool       `json:"active"`
	RoleError     string     `json:"roleError,omitempty"`
	// NoticeError is why the player was not told by direct message ("" = they were). Not stored.
	NoticeError string `json:"noticeError,omitempty"`
}

const vipMemberCols = `m.id,m.tier_id,t.name,m.player_id,p.display_name,COALESCE(m.discord_user_id,''),COALESCE(t.discord_role_id,''),g.discord_guild_id,
m.note,m.granted_by,m.granted_at,m.expires_at,m.revoked_at,COALESCE(m.revoked_reason,''),(` + vipActive + `),COALESCE(m.role_error,'')`

const vipMemberFrom = ` FROM vip_members m JOIN vip_tiers t ON t.id=m.tier_id JOIN players p ON p.id=m.player_id JOIN guilds g ON g.id=m.guild_id`

func scanVIPMember(row pgx.Row) (VIPMember, error) {
	var m VIPMember
	err := row.Scan(&m.ID, &m.TierID, &m.TierName, &m.PlayerID, &m.PlayerName, &m.DiscordUserID, &m.RoleID, &m.DiscordGuild, &m.Note, &m.GrantedBy, &m.GrantedAt, &m.ExpiresAt, &m.RevokedAt, &m.RevokedReason, &m.Active, &m.RoleError)
	return m, err
}

func (r *VIPRepository) collect(rows pgx.Rows, err error) ([]VIPMember, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VIPMember{}
	for rows.Next() {
		m, err := scanVIPMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListMembers returns active memberships first, then the most recently ended.
func (r *VIPRepository) ListMembers(ctx context.Context, guildID int64, limit int) ([]VIPMember, error) {
	return r.collect(r.pool.Query(ctx, `SELECT `+vipMemberCols+vipMemberFrom+` WHERE m.guild_id=$1
ORDER BY (`+vipActive+`) DESC, COALESCE(m.revoked_at,m.granted_at) DESC LIMIT $2`, guildID, clampLimit(limit, 100, 500)))
}

// Grant gives a player a tier. The Discord account comes from the player's verified link.
func (r *VIPRepository) Grant(ctx context.Context, guildID, tierID, playerID int64, expiresAt *time.Time, note, by string) (VIPMember, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return VIPMember{}, err
	}
	defer tx.Rollback(ctx)
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM vip_tiers WHERE guild_id=$1 AND id=$2)`, guildID, tierID).Scan(&ok); err != nil {
		return VIPMember{}, err
	}
	if !ok {
		return VIPMember{}, ErrVIPTierNotFound
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM players WHERE guild_id=$1 AND id=$2)`, guildID, playerID).Scan(&ok); err != nil {
		return VIPMember{}, err
	}
	if !ok {
		return VIPMember{}, ErrVIPPlayerNotFound
	}
	// An expired-but-not-revoked membership is closed first so it never blocks a new grant.
	if _, err := tx.Exec(ctx, `UPDATE vip_members SET revoked_at=expires_at,revoked_reason='EXPIRED' WHERE guild_id=$1 AND player_id=$2 AND revoked_at IS NULL AND expires_at IS NOT NULL AND expires_at<=NOW()`, guildID, playerID); err != nil {
		return VIPMember{}, err
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO vip_members(guild_id,tier_id,player_id,discord_user_id,note,granted_by,expires_at)
VALUES($1,$2,$3,(SELECT discord_user_id FROM player_links WHERE guild_id=$1 AND player_id=$3 AND status='VERIFIED' LIMIT 1),$4,$5,$6) RETURNING id`,
		guildID, tierID, playerID, note, by, expiresAt).Scan(&id)
	if isUniqueViolation(err) {
		return VIPMember{}, ErrVIPAlreadyMember
	}
	if err != nil {
		return VIPMember{}, err
	}
	m, err := scanVIPMember(tx.QueryRow(ctx, `SELECT `+vipMemberCols+vipMemberFrom+` WHERE m.id=$1`, id))
	if err != nil {
		return VIPMember{}, err
	}
	return m, tx.Commit(ctx)
}

// Revoke ends an active membership.
func (r *VIPRepository) Revoke(ctx context.Context, guildID, memberID int64, reason string) (VIPMember, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE vip_members SET revoked_at=NOW(),revoked_reason=$3 WHERE guild_id=$1 AND id=$2 AND revoked_at IS NULL`, guildID, memberID, reason)
	if err != nil {
		return VIPMember{}, err
	}
	if tag.RowsAffected() == 0 {
		return VIPMember{}, ErrVIPMemberNotFound
	}
	return scanVIPMember(r.pool.QueryRow(ctx, `SELECT `+vipMemberCols+vipMemberFrom+` WHERE m.id=$1`, memberID))
}

// ExpireDue closes memberships whose time ran out and returns them (for Discord role removal).
func (r *VIPRepository) ExpireDue(ctx context.Context, guildID int64, now time.Time) ([]VIPMember, error) {
	rows, err := r.pool.Query(ctx, `UPDATE vip_members SET revoked_at=expires_at,revoked_reason='EXPIRED'
WHERE guild_id=$1 AND revoked_at IS NULL AND expires_at IS NOT NULL AND expires_at<=$2 RETURNING id`, guildID, now)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return []VIPMember{}, err
	}
	return r.collect(r.pool.Query(ctx, `SELECT `+vipMemberCols+vipMemberFrom+` WHERE m.id = ANY($1)`, ids))
}

// RecordRoleSync stores the outcome of the last Discord role add/remove ("" = success).
func (r *VIPRepository) RecordRoleSync(ctx context.Context, memberID int64, problem string) error {
	_, err := r.pool.Exec(ctx, `UPDATE vip_members SET role_synced_at=NOW(),role_error=NULLIF($2,'') WHERE id=$1`, memberID, problem)
	return err
}

// ActiveBadge is the killfeed badge of a player's active tier ("" when none).
func (r *VIPRepository) ActiveBadge(ctx context.Context, guildID, playerID int64) (string, error) {
	var badge string
	err := r.pool.QueryRow(ctx, `SELECT t.badge FROM vip_members m JOIN vip_tiers t ON t.id=m.tier_id
WHERE m.guild_id=$1 AND m.player_id=$2 AND `+vipActive+` LIMIT 1`, guildID, playerID).Scan(&badge)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return badge, err
}

// Multipliers returns the reward multiplier of every player with an active tier above 1.0.
func (r *VIPRepository) Multipliers(ctx context.Context, guildID int64) (map[int64]float64, error) {
	rows, err := r.pool.Query(ctx, `SELECT m.player_id,t.reward_multiplier::float8 FROM vip_members m JOIN vip_tiers t ON t.id=m.tier_id
WHERE m.guild_id=$1 AND t.reward_multiplier > 1 AND `+vipActive, guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]float64{}
	for rows.Next() {
		var id int64
		var mult float64
		if err := rows.Scan(&id, &mult); err != nil {
			return nil, err
		}
		out[id] = mult
	}
	return out, rows.Err()
}

// GrantOrExtend gives a player a tier until expiresAt (nil = no end), or, when they already hold
// that same tier, moves its end later (a membership with no end stays that way). created reports
// whether a new membership was made. Holding a different tier is ErrVIPAlreadyMember.
func (r *VIPRepository) GrantOrExtend(ctx context.Context, guildID, tierID, playerID int64, expiresAt *time.Time, note, by string) (m VIPMember, created bool, err error) {
	var id, heldTier int64
	err = r.pool.QueryRow(ctx, `SELECT m.id,m.tier_id FROM vip_members m WHERE m.guild_id=$1 AND m.player_id=$2 AND `+vipActive, guildID, playerID).Scan(&id, &heldTier)
	if errors.Is(err, pgx.ErrNoRows) {
		m, err = r.Grant(ctx, guildID, tierID, playerID, expiresAt, note, by)
		return m, err == nil, err
	}
	if err != nil {
		return VIPMember{}, false, err
	}
	if heldTier != tierID {
		return VIPMember{}, false, ErrVIPAlreadyMember
	}
	if _, err = r.pool.Exec(ctx, `UPDATE vip_members SET expires_at=CASE WHEN expires_at IS NULL OR $2::TIMESTAMPTZ IS NULL THEN NULL ELSE GREATEST(expires_at,$2) END WHERE id=$1`, id, expiresAt); err != nil {
		return VIPMember{}, false, err
	}
	m, err = scanVIPMember(r.pool.QueryRow(ctx, `SELECT `+vipMemberCols+vipMemberFrom+` WHERE m.id=$1`, id))
	return m, false, err
}

// RevokeIfActive ends a membership by id when it is still open; ok=false when it had already ended.
func (r *VIPRepository) RevokeIfActive(ctx context.Context, guildID, memberID int64, reason string) (VIPMember, bool, error) {
	m, err := r.Revoke(ctx, guildID, memberID, reason)
	if errors.Is(err, ErrVIPMemberNotFound) {
		return VIPMember{}, false, nil
	}
	return m, err == nil, err
}

// PlayerTier is the supporter tier a player holds, as the Player Hub and the grant notice show it.
type PlayerTier struct {
	TierID           int64      `json:"tierId"`
	Name             string     `json:"name"`
	Badge            string     `json:"badge"`
	Color            string     `json:"color"`
	RewardMultiplier float64    `json:"rewardMultiplier"`
	DiscordRole      bool       `json:"discordRole"`
	GrantedAt        time.Time  `json:"grantedAt"`
	ExpiresAt        *time.Time `json:"expiresAt"`
}

// ActiveForPlayer returns the tier a player holds right now, or nil when they hold none.
func (r *VIPRepository) ActiveForPlayer(ctx context.Context, guildID, playerID int64) (*PlayerTier, error) {
	var t PlayerTier
	err := r.pool.QueryRow(ctx, `SELECT t.id,t.name,t.badge,t.color,t.reward_multiplier,COALESCE(t.discord_role_id,'')<>'',m.granted_at,m.expires_at
FROM vip_members m JOIN vip_tiers t ON t.id=m.tier_id WHERE m.guild_id=$1 AND m.player_id=$2 AND `+vipActive+` LIMIT 1`, guildID, playerID).
		Scan(&t.TierID, &t.Name, &t.Badge, &t.Color, &t.RewardMultiplier, &t.DiscordRole, &t.GrantedAt, &t.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}
