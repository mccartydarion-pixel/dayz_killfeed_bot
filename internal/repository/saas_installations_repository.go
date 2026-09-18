package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Installation statuses. Enforced at the application layer, matching this
// schema's existing convention for status-like text columns (no SQL CHECK
// constraint - see the role constants in saas_organizations_repository.go).
const (
	InstallationNotStarted       = "NOT_STARTED"
	InstallationDiscordConnected = "DISCORD_CONNECTED"
	InstallationNitradoConnected = "NITRADO_CONNECTED"
	InstallationConfiguring      = "CONFIGURING"
	InstallationReady            = "READY"
	InstallationDegraded         = "DEGRADED"
	InstallationDisconnected     = "DISCONNECTED"
	InstallationSuspended        = "SUSPENDED"
)

// Installation connects an organization, a Discord guild connection, and (once
// selected) a DayZ server - the unit the onboarding flow and dashboard
// operate on.
type Installation struct {
	ID                       int64
	OrganizationID           int64
	DiscordGuildConnectionID int64
	GameServerID             *int64
	Status, Plan             string
	CreatedAt, UpdatedAt     time.Time
	SetupCompletedAt         *time.Time
	LastHealthCheckAt        *time.Time
}

// InstallationSetupProgress is resumable onboarding state for one
// installation. Only durable, non-derivable progress is stored here (section
// 10) - anything the UI can recompute from Installation/GuildConnection/
// GameServer state does not belong here.
type InstallationSetupProgress struct {
	InstallationID                                                                             int64
	CurrentStep                                                                                string
	DiscordCompleted, NitradoCompleted, ServerSelected, ChannelsCompleted, ValidationCompleted bool
	CompletedAt                                                                                *time.Time
	UpdatedAt                                                                                  time.Time
}

// InstallationSettings is the SaaS dashboard-facing settings surface for one
// installation. It is intentionally decoupled from guilds' own
// killfeed/leaderboards/player-stats channel columns (which remain
// authoritative for the current single-server-per-guild bot runtime): an
// installation is a (guild, server) pair, so its settings are the forward
// path to per-server channel configuration once a guild has multiple DayZ
// servers. See docs/SAAS_SCHEMA.md.
type InstallationSettings struct {
	InstallationID                                                                    int64
	KillfeedChannelID, LeaderboardChannelID, PlayerStatusChannelID, AdminLogChannelID string
	Timezone, DistanceUnit                                                            string
	OnlineDisplayEnabled, LeaderboardEnabled                                          bool
	CreatedAt, UpdatedAt                                                              time.Time
}

type InstallationRepository struct{ pool *pgxpool.Pool }

func NewInstallationRepository(pool *pgxpool.Pool) *InstallationRepository {
	return &InstallationRepository{pool: pool}
}

// Create inserts a new installation together with its setup-progress and
// settings child rows, atomically (section 17): an installation must never
// exist without them, since GetSetupProgress/GetSettings assume they do.
func (r *InstallationRepository) Create(ctx context.Context, organizationID, discordGuildConnectionID int64, gameServerID *int64) (*Installation, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin create installation: %w", err)
	}
	defer tx.Rollback(ctx)

	var out Installation
	const q = `
INSERT INTO installations(organization_id, discord_guild_connection_id, game_server_id, status)
VALUES($1,$2,$3,$4)
RETURNING id, organization_id, discord_guild_connection_id, game_server_id, status, COALESCE(plan,''), created_at, updated_at, setup_completed_at, last_health_check_at`
	err = tx.QueryRow(ctx, q, organizationID, discordGuildConnectionID, gameServerID, InstallationNotStarted).
		Scan(&out.ID, &out.OrganizationID, &out.DiscordGuildConnectionID, &out.GameServerID, &out.Status, &out.Plan, &out.CreatedAt, &out.UpdatedAt, &out.SetupCompletedAt, &out.LastHealthCheckAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		return nil, fmt.Errorf("create installation: %w", err)
	}

	if _, err := tx.Exec(ctx, `INSERT INTO installation_setup_progress(installation_id) VALUES($1)`, out.ID); err != nil {
		return nil, fmt.Errorf("create installation setup progress: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO installation_settings(installation_id) VALUES($1)`, out.ID); err != nil {
		return nil, fmt.Errorf("create installation settings: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit create installation: %w", err)
	}
	return &out, nil
}

// GetScoped returns one installation, requiring it belong to organizationID
// (section 15 tenant isolation).
func (r *InstallationRepository) GetScoped(ctx context.Context, organizationID, installationID int64) (*Installation, error) {
	const q = `SELECT id, organization_id, discord_guild_connection_id, game_server_id, status, COALESCE(plan,''), created_at, updated_at, setup_completed_at, last_health_check_at FROM installations WHERE organization_id=$1 AND id=$2`
	var out Installation
	err := r.pool.QueryRow(ctx, q, organizationID, installationID).
		Scan(&out.ID, &out.OrganizationID, &out.DiscordGuildConnectionID, &out.GameServerID, &out.Status, &out.Plan, &out.CreatedAt, &out.UpdatedAt, &out.SetupCompletedAt, &out.LastHealthCheckAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get scoped installation: %w", err)
	}
	return &out, nil
}

// ListByOrganization returns every installation belonging to organizationID.
func (r *InstallationRepository) ListByOrganization(ctx context.Context, organizationID int64) ([]Installation, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, organization_id, discord_guild_connection_id, game_server_id, status, COALESCE(plan,''), created_at, updated_at, setup_completed_at, last_health_check_at FROM installations WHERE organization_id=$1 ORDER BY id`, organizationID)
	if err != nil {
		return nil, fmt.Errorf("list installations for organization: %w", err)
	}
	defer rows.Close()
	var out []Installation
	for rows.Next() {
		var i Installation
		if err := rows.Scan(&i.ID, &i.OrganizationID, &i.DiscordGuildConnectionID, &i.GameServerID, &i.Status, &i.Plan, &i.CreatedAt, &i.UpdatedAt, &i.SetupCompletedAt, &i.LastHealthCheckAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// UpdateStatus transitions installationID's status, requiring it belong to
// organizationID (section 15). Stamps setup_completed_at the first time
// status reaches InstallationReady.
func (r *InstallationRepository) UpdateStatus(ctx context.Context, organizationID, installationID int64, status string) error {
	const q = `
UPDATE installations SET status=$3, updated_at=NOW(),
    setup_completed_at = CASE WHEN $3='READY' AND setup_completed_at IS NULL THEN NOW() ELSE setup_completed_at END
WHERE organization_id=$1 AND id=$2`
	if _, err := r.pool.Exec(ctx, q, organizationID, installationID, status); err != nil {
		return fmt.Errorf("update installation status: %w", err)
	}
	return nil
}

// SetGameServer associates gameServerID (a game_servers row, see
// SaaSServerRepository.UpsertForInstallation) with installationID, requiring
// it belong to organizationID (section 15). This is the "select dayz-server"
// step - it never touches status/setup progress itself, those are updated
// separately by the caller.
func (r *InstallationRepository) SetGameServer(ctx context.Context, organizationID, installationID, gameServerID int64) error {
	if _, err := r.pool.Exec(ctx, `UPDATE installations SET game_server_id=$3, updated_at=NOW() WHERE organization_id=$1 AND id=$2`, organizationID, installationID, gameServerID); err != nil {
		return fmt.Errorf("set installation game server: %w", err)
	}
	return nil
}

// RecordHealthCheck stamps last_health_check_at, requiring installationID
// belong to organizationID (section 15).
func (r *InstallationRepository) RecordHealthCheck(ctx context.Context, organizationID, installationID int64, at time.Time) error {
	if _, err := r.pool.Exec(ctx, `UPDATE installations SET last_health_check_at=$3, updated_at=NOW() WHERE organization_id=$1 AND id=$2`, organizationID, installationID, at); err != nil {
		return fmt.Errorf("record installation health check: %w", err)
	}
	return nil
}

// GetSetupProgress returns installationID's onboarding progress, requiring
// it belong to organizationID (section 15).
func (r *InstallationRepository) GetSetupProgress(ctx context.Context, organizationID, installationID int64) (*InstallationSetupProgress, error) {
	const q = `
SELECT p.installation_id, p.current_step, p.discord_completed, p.nitrado_completed, p.server_selected, p.channels_completed, p.validation_completed, p.completed_at, p.updated_at
FROM installation_setup_progress p
JOIN installations i ON i.id = p.installation_id
WHERE i.organization_id=$1 AND p.installation_id=$2`
	var out InstallationSetupProgress
	err := r.pool.QueryRow(ctx, q, organizationID, installationID).
		Scan(&out.InstallationID, &out.CurrentStep, &out.DiscordCompleted, &out.NitradoCompleted, &out.ServerSelected, &out.ChannelsCompleted, &out.ValidationCompleted, &out.CompletedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get setup progress: %w", err)
	}
	return &out, nil
}

// UpdateSetupProgress persists resumable onboarding state, requiring
// installationID belong to organizationID (section 15). completed_at is
// stamped the first time ValidationCompleted becomes true.
func (r *InstallationRepository) UpdateSetupProgress(ctx context.Context, organizationID, installationID int64, p InstallationSetupProgress) error {
	const q = `
UPDATE installation_setup_progress p SET
    current_step=$3, discord_completed=$4, nitrado_completed=$5, server_selected=$6, channels_completed=$7, validation_completed=$8,
    completed_at = CASE WHEN $8 AND p.completed_at IS NULL THEN NOW() ELSE p.completed_at END,
    updated_at=NOW()
FROM installations i
WHERE i.id = p.installation_id AND i.organization_id=$1 AND p.installation_id=$2`
	_, err := r.pool.Exec(ctx, q, organizationID, installationID, p.CurrentStep, p.DiscordCompleted, p.NitradoCompleted, p.ServerSelected, p.ChannelsCompleted, p.ValidationCompleted)
	if err != nil {
		return fmt.Errorf("update setup progress: %w", err)
	}
	return nil
}

// GetSettings returns installationID's settings, requiring it belong to
// organizationID (section 15).
func (r *InstallationRepository) GetSettings(ctx context.Context, organizationID, installationID int64) (*InstallationSettings, error) {
	const q = `
SELECT s.installation_id, COALESCE(s.killfeed_channel_id,''), COALESCE(s.leaderboard_channel_id,''), COALESCE(s.player_status_channel_id,''), COALESCE(s.admin_log_channel_id,''), s.timezone, s.distance_unit, s.online_display_enabled, s.leaderboard_enabled, s.created_at, s.updated_at
FROM installation_settings s
JOIN installations i ON i.id = s.installation_id
WHERE i.organization_id=$1 AND s.installation_id=$2`
	var out InstallationSettings
	err := r.pool.QueryRow(ctx, q, organizationID, installationID).
		Scan(&out.InstallationID, &out.KillfeedChannelID, &out.LeaderboardChannelID, &out.PlayerStatusChannelID, &out.AdminLogChannelID, &out.Timezone, &out.DistanceUnit, &out.OnlineDisplayEnabled, &out.LeaderboardEnabled, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get installation settings: %w", err)
	}
	return &out, nil
}

// UpdateSettings persists installationID's settings, requiring it belong to
// organizationID (section 15).
func (r *InstallationRepository) UpdateSettings(ctx context.Context, organizationID, installationID int64, s InstallationSettings) error {
	const q = `
UPDATE installation_settings s SET
    killfeed_channel_id=$3, leaderboard_channel_id=$4, player_status_channel_id=$5, admin_log_channel_id=$6,
    timezone=$7, distance_unit=$8, online_display_enabled=$9, leaderboard_enabled=$10, updated_at=NOW()
FROM installations i
WHERE i.id = s.installation_id AND i.organization_id=$1 AND s.installation_id=$2`
	_, err := r.pool.Exec(ctx, q, organizationID, installationID,
		emptyToNil(s.KillfeedChannelID), emptyToNil(s.LeaderboardChannelID), emptyToNil(s.PlayerStatusChannelID), emptyToNil(s.AdminLogChannelID),
		s.Timezone, s.DistanceUnit, s.OnlineDisplayEnabled, s.LeaderboardEnabled)
	if err != nil {
		return fmt.Errorf("update installation settings: %w", err)
	}
	return nil
}
