package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FactionSecurityRepository backs Faction Security: the server owner's switch,
// the base owner's choice of who in their faction is told, finding those
// members, and the share log.
type FactionSecurityRepository struct{ pool *pgxpool.Pool }

func NewFactionSecurityRepository(pool *pgxpool.Pool) *FactionSecurityRepository {
	return &FactionSecurityRepository{pool: pool}
}

const (
	ServiceFactionSecurity = "FACTION_SECURITY"

	FactionRecipientsAll     = "ALL"
	FactionRecipientsLeaders = "LEADERS"

	FactionShareRaidAlarm      = "RAID_ALARM"
	FactionSharePerimeterWatch = "PERIMETER_WATCH"

	// Never message more than this many faction members about one alert.
	FactionSecurityMaxRecipients = 15
)

var ErrInvalidFactionSecurity = errors.New("invalid faction security request")

type FactionSecuritySettings struct {
	Enabled   bool       `json:"enabled"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

type FactionSecurityShare struct {
	ID        int64     `json:"id"`
	BaseID    int64     `json:"baseId"`
	BaseName  string    `json:"baseName"`
	Source    string    `json:"source"`
	Sent      int       `json:"sent"`
	Failed    int       `json:"failed"`
	CreatedAt time.Time `json:"createdAt"`
}

// FactionSecuritySummary is what a base owner sees about who would be told.
type FactionSecuritySummary struct {
	InFaction     bool   `json:"inFaction"`
	FactionName   string `json:"factionName,omitempty"`
	LinkedMembers int    `json:"linkedMembers"`
	LinkedLeaders int    `json:"linkedLeaders"`
}

func (r *FactionSecurityRepository) ready() bool { return r != nil && r.pool != nil }

// GetSettings returns the switch for one server. No row means off.
func (r *FactionSecurityRepository) GetSettings(ctx context.Context, installationID, guildID, serverID int64) (FactionSecuritySettings, error) {
	var out FactionSecuritySettings
	if !r.ready() || installationID <= 0 || guildID <= 0 || serverID <= 0 {
		return out, ErrInvalidFactionSecurity
	}
	var updated time.Time
	err := r.pool.QueryRow(ctx, `SELECT enabled,updated_at FROM faction_security_settings
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3`, installationID, guildID, serverID).Scan(&out.Enabled, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.UpdatedAt = &updated
	return out, nil
}

// SetSettings stores the switch; the foreign keys reject a mismatched scope.
func (r *FactionSecurityRepository) SetSettings(ctx context.Context, installationID, guildID, serverID int64, enabled bool, actorUserID *int64) (FactionSecuritySettings, error) {
	if !r.ready() || installationID <= 0 || guildID <= 0 || serverID <= 0 {
		return FactionSecuritySettings{}, ErrInvalidFactionSecurity
	}
	var out FactionSecuritySettings
	var updated time.Time
	err := r.pool.QueryRow(ctx, `INSERT INTO faction_security_settings
 (installation_id,guild_id,server_id,enabled,updated_by_user_id,updated_at) VALUES ($1,$2,$3,$4,$5,NOW())
 ON CONFLICT (installation_id,server_id) DO UPDATE SET enabled=EXCLUDED.enabled,
  updated_by_user_id=EXCLUDED.updated_by_user_id,updated_at=NOW()
 WHERE faction_security_settings.guild_id=EXCLUDED.guild_id
 RETURNING enabled,updated_at`, installationID, guildID, serverID, enabled, actorUserID).Scan(&out.Enabled, &updated)
	if err != nil {
		return FactionSecuritySettings{}, err
	}
	out.UpdatedAt = &updated
	return out, nil
}

// GetPreference returns who in the base owner's faction is told (default everyone).
func (r *FactionSecurityRepository) GetPreference(ctx context.Context, installationID, playerID int64) (string, error) {
	if !r.ready() || installationID <= 0 || playerID <= 0 {
		return "", ErrInvalidFactionSecurity
	}
	var who string
	err := r.pool.QueryRow(ctx, `SELECT recipients FROM faction_security_preferences WHERE installation_id=$1 AND player_id=$2`,
		installationID, playerID).Scan(&who)
	if errors.Is(err, pgx.ErrNoRows) {
		return FactionRecipientsAll, nil
	}
	return who, err
}

// SetPreference stores the base owner's choice.
func (r *FactionSecurityRepository) SetPreference(ctx context.Context, installationID, playerID int64, recipients string) error {
	if !r.ready() || installationID <= 0 || playerID <= 0 || (recipients != FactionRecipientsAll && recipients != FactionRecipientsLeaders) {
		return ErrInvalidFactionSecurity
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO faction_security_preferences(installation_id,player_id,recipients,updated_at)
 VALUES ($1,$2,$3,NOW())
 ON CONFLICT (installation_id,player_id) DO UPDATE SET recipients=EXCLUDED.recipients,updated_at=NOW()`,
		installationID, playerID, recipients)
	return err
}

// Recipients returns the verified Discord accounts of the base owner's active
// faction mates who should be told about an alert on their base, when Faction
// Security is on for the server and (while it is on sale) the base owner has
// paid time. It honours the owner's choice of everyone or only leaders, never
// includes the owner and returns at most FactionSecurityMaxRecipients.
func (r *FactionSecurityRepository) Recipients(ctx context.Context, installationID, guildID, serverID, ownerPlayerID int64) ([]string, error) {
	if !r.ready() || installationID <= 0 || guildID <= 0 || serverID <= 0 || ownerPlayerID <= 0 {
		return nil, ErrInvalidFactionSecurity
	}
	rows, err := r.pool.Query(ctx, `
WITH owner_faction AS (
 SELECT faction_id FROM faction_members WHERE guild_id=$2 AND player_id=$4 AND active
), owner_link AS (
 SELECT discord_user_id FROM player_links WHERE guild_id=$2 AND player_id=$4 AND status='VERIFIED'
), pref AS (
 SELECT COALESCE((SELECT recipients FROM faction_security_preferences WHERE installation_id=$1 AND player_id=$4),'ALL') AS who
)
SELECT DISTINCT pl.discord_user_id
FROM faction_members fm
JOIN player_links pl ON pl.guild_id=fm.guild_id AND pl.player_id=fm.player_id AND pl.status='VERIFIED'
CROSS JOIN pref
WHERE fm.guild_id=$2 AND fm.active AND fm.player_id<>$4
 AND fm.faction_id IN (SELECT faction_id FROM owner_faction)
 AND (pref.who='ALL' OR fm.role IN ('OWNER','LEADER','OFFICER'))
 AND pl.discord_user_id<>'' AND pl.discord_user_id NOT IN (SELECT discord_user_id FROM owner_link)
 AND EXISTS (SELECT 1 FROM faction_security_settings s
  WHERE s.installation_id=$1 AND s.guild_id=$2 AND s.server_id=$3 AND s.enabled)
 AND (NOT EXISTS (
  SELECT 1 FROM security_service_offers o
  WHERE o.installation_id=$1 AND o.server_id=$3 AND o.service_id IN ('FACTION_SECURITY','SENTINEL_PRO') AND o.enabled)
  OR EXISTS (
  SELECT 1 FROM security_service_purchases sp
  WHERE sp.installation_id=$1 AND sp.player_id=$4 AND sp.service_id IN ('FACTION_SECURITY','SENTINEL_PRO')
   AND sp.starts_at<=NOW() AND sp.ends_at>NOW()))
ORDER BY 1 LIMIT $5`, installationID, guildID, serverID, ownerPlayerID, FactionSecurityMaxRecipients)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// LogShare records one shared alert.
func (r *FactionSecurityRepository) LogShare(ctx context.Context, installationID, guildID, serverID, baseID int64, source string, sent, failed int) error {
	if !r.ready() || baseID <= 0 || (source != FactionShareRaidAlarm && source != FactionSharePerimeterWatch) || sent < 0 || failed < 0 {
		return ErrInvalidFactionSecurity
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO faction_security_shares(installation_id,guild_id,server_id,base_id,source,sent,failed)
 VALUES ($1,$2,$3,$4,$5,$6,$7)`, installationID, guildID, serverID, baseID, source, sent, failed)
	return err
}

// RecentShares lists the newest shared alerts for one server, for the server owner.
func (r *FactionSecurityRepository) RecentShares(ctx context.Context, installationID, guildID, serverID int64, limit int) ([]FactionSecurityShare, error) {
	if !r.ready() || installationID <= 0 || guildID <= 0 || serverID <= 0 {
		return nil, ErrInvalidFactionSecurity
	}
	if limit < 1 || limit > 50 {
		limit = 10
	}
	rows, err := r.pool.Query(ctx, `SELECT s.id,s.base_id,b.name,s.source,s.sent,s.failed,s.created_at
 FROM faction_security_shares s
 JOIN case_registered_bases b ON b.installation_id=s.installation_id AND b.guild_id=s.guild_id AND b.server_id=s.server_id AND b.id=s.base_id
 WHERE s.installation_id=$1 AND s.guild_id=$2 AND s.server_id=$3
 ORDER BY s.created_at DESC,s.id DESC LIMIT $4`, installationID, guildID, serverID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]FactionSecurityShare, 0)
	for rows.Next() {
		var s FactionSecurityShare
		if err := rows.Scan(&s.ID, &s.BaseID, &s.BaseName, &s.Source, &s.Sent, &s.Failed, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Summary tells a base owner their faction and how many of its members have a
// verified Discord link (everyone, and leaders only), not counting the owner.
func (r *FactionSecurityRepository) Summary(ctx context.Context, guildID, playerID int64) (FactionSecuritySummary, error) {
	var out FactionSecuritySummary
	if !r.ready() || guildID <= 0 || playerID <= 0 {
		return out, ErrInvalidFactionSecurity
	}
	var factionID int64
	err := r.pool.QueryRow(ctx, `SELECT f.id,f.name FROM faction_members fm JOIN factions f ON f.id=fm.faction_id
 WHERE fm.guild_id=$1 AND fm.player_id=$2 AND fm.active`, guildID, playerID).Scan(&factionID, &out.FactionName)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.InFaction = true
	err = r.pool.QueryRow(ctx, `SELECT COUNT(DISTINCT fm.player_id),
  COUNT(DISTINCT fm.player_id) FILTER (WHERE fm.role IN ('OWNER','LEADER','OFFICER'))
 FROM faction_members fm
 JOIN player_links pl ON pl.guild_id=fm.guild_id AND pl.player_id=fm.player_id AND pl.status='VERIFIED'
 WHERE fm.guild_id=$1 AND fm.faction_id=$2 AND fm.active AND fm.player_id<>$3`, guildID, factionID, playerID).
		Scan(&out.LinkedMembers, &out.LinkedLeaders)
	return out, err
}
