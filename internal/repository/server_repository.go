package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type GameServer struct {
	ID, GuildID                                                      int64
	Provider, ProviderServiceID, Game, Platform, DisplayName, Status string
	Active                                                           bool
	CreatedAt, UpdatedAt                                             time.Time
	// OrganizationID links this server to a SaaS organization (see
	// internal/repository/saas_installations_repository.go). Nil until the
	// SaaS onboarding flow claims this server - existing guild-scoped
	// methods on ServerRepository never set or select it.
	OrganizationID *int64
}
type NitradoConnection struct {
	ID, GuildID                                   int64
	Ciphertext, Nonce                             []byte
	KeyVersion                                    int
	Status                                        string
	LastValidatedAt, LastSuccessAt, LastFailureAt *time.Time
	LastErrorClass                                string
	// OrganizationID links this credential envelope to a SaaS organization
	// (see internal/repository/saas_credentials_repository.go). Nil until
	// claimed by the SaaS onboarding flow.
	OrganizationID *int64
}
type ServerConfig struct {
	ServerID                                                                                                        int64
	PollIntervalMS, RescanSeconds                                                                                   int
	StartupMode                                                                                                     string
	PublishPvPKills, PublishSuicides, PublishUnknownDeaths, OnlineCounterEnabled, KillfeedEnabled, AnalyticsEnabled bool
	ADMMonitorMessageID                                                                                             string
}
type ServerRepository struct{ pool *pgxpool.Pool }

func NewServerRepository(pool *pgxpool.Pool) *ServerRepository { return &ServerRepository{pool: pool} }
func (r *ServerRepository) UpsertGameServer(ctx context.Context, s GameServer) (*GameServer, error) {
	var out GameServer
	err := r.pool.QueryRow(ctx, `INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,display_name,status,active) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(guild_id,provider,provider_service_id) DO UPDATE SET display_name=EXCLUDED.display_name,status=EXCLUDED.status,active=EXCLUDED.active,updated_at=NOW() RETURNING id,guild_id,provider,provider_service_id,game,platform,COALESCE(display_name,''),status,active,created_at,updated_at`, s.GuildID, s.Provider, s.ProviderServiceID, s.Game, s.Platform, s.DisplayName, s.Status, s.Active).Scan(&out.ID, &out.GuildID, &out.Provider, &out.ProviderServiceID, &out.Game, &out.Platform, &out.DisplayName, &out.Status, &out.Active, &out.CreatedAt, &out.UpdatedAt)
	return &out, err
}
func (r *ServerRepository) ListActive(ctx context.Context) ([]GameServer, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,guild_id,provider,provider_service_id,game,platform,COALESCE(display_name,''),status,active,created_at,updated_at FROM game_servers WHERE active ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GameServer
	for rows.Next() {
		var s GameServer
		if err := rows.Scan(&s.ID, &s.GuildID, &s.Provider, &s.ProviderServiceID, &s.Game, &s.Platform, &s.DisplayName, &s.Status, &s.Active, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ConnectedServerID resolves the one server bound to guildID. Binding is the
// active flag - the same predicate the runtime uses to start workers
// (ListActive/ListActiveByGuild) - not status, which the SaaS dashboard
// writes as Nitrado power state (ONLINE/OFFLINE, see nitradoServiceStatus).
// Only an explicit DISCONNECTED teardown row is excluded.
func (r *ServerRepository) ConnectedServerID(ctx context.Context, guildID int64) (int64, error) {
	rows, err := r.pool.Query(ctx, `SELECT id FROM game_servers WHERE guild_id=$1 AND active AND UPPER(COALESCE(status,'')) <> 'DISCONNECTED' ORDER BY id`, guildID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	switch len(ids) {
	case 0:
		return 0, fmt.Errorf("no connected server for guild %d", guildID)
	case 1:
		return ids[0], nil
	default:
		return 0, fmt.Errorf("multiple connected servers for guild %d; explicit server selection required", guildID)
	}
}

// ListActiveByGuild returns every active game_servers row for one guild. This is
// the enumeration source for starting one worker per server (multi-server runtime).
func (r *ServerRepository) ListActiveByGuild(ctx context.Context, guildID int64) ([]GameServer, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,guild_id,provider,provider_service_id,game,platform,COALESCE(display_name,''),status,active,created_at,updated_at FROM game_servers WHERE guild_id=$1 AND active ORDER BY id`, guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GameServer
	for rows.Next() {
		var s GameServer
		if err := rows.Scan(&s.ID, &s.GuildID, &s.Provider, &s.ProviderServiceID, &s.Game, &s.Platform, &s.DisplayName, &s.Status, &s.Active, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetByID looks up a single game_servers row, used when a worker needs to
// re-resolve its server record (e.g. after the initial enumeration snapshot).
func (r *ServerRepository) GetByID(ctx context.Context, id int64) (*GameServer, error) {
	var s GameServer
	err := r.pool.QueryRow(ctx, `SELECT id,guild_id,provider,provider_service_id,game,platform,COALESCE(display_name,''),status,active,created_at,updated_at FROM game_servers WHERE id=$1`, id).Scan(&s.ID, &s.GuildID, &s.Provider, &s.ProviderServiceID, &s.Game, &s.Platform, &s.DisplayName, &s.Status, &s.Active, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// FindByGuildAndService looks up a game_servers row by its provider service ID
// regardless of active state, for onboarding flows (disconnect/repair) that
// need to find a server that may already be inactive.
func (r *ServerRepository) FindByGuildAndService(ctx context.Context, guildID int64, providerServiceID string) (*GameServer, error) {
	var s GameServer
	err := r.pool.QueryRow(ctx, `SELECT id,guild_id,provider,provider_service_id,game,platform,COALESCE(display_name,''),status,active,created_at,updated_at FROM game_servers WHERE guild_id=$1 AND provider_service_id=$2`, guildID, providerServiceID).Scan(&s.ID, &s.GuildID, &s.Provider, &s.ProviderServiceID, &s.Game, &s.Platform, &s.DisplayName, &s.Status, &s.Active, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Deactivate marks a game_servers row inactive (clean teardown) while keeping
// its history (kills/deaths/activity) intact for later reconnection.
func (r *ServerRepository) Deactivate(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE game_servers SET active=FALSE, status='DISCONNECTED', updated_at=NOW() WHERE id=$1`, id)
	return err
}

// Reactivate marks a previously disconnected game_servers row active again
// (used by /server repair and /server select re-connecting an existing row).
func (r *ServerRepository) Reactivate(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE game_servers SET active=TRUE, status='CONNECTED', updated_at=NOW() WHERE id=$1`, id)
	return err
}
func (r *ServerRepository) SaveConnection(ctx context.Context, c NitradoConnection) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO nitrado_connections(guild_id,credential_ciphertext,credential_nonce,credential_key_version,status,last_validated_at,last_success_at,last_failure_at,last_error_class) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(guild_id) DO UPDATE SET credential_ciphertext=EXCLUDED.credential_ciphertext,credential_nonce=EXCLUDED.credential_nonce,credential_key_version=EXCLUDED.credential_key_version,status=EXCLUDED.status,last_validated_at=EXCLUDED.last_validated_at,last_success_at=EXCLUDED.last_success_at,last_failure_at=EXCLUDED.last_failure_at,last_error_class=EXCLUDED.last_error_class,updated_at=NOW()`, c.GuildID, c.Ciphertext, c.Nonce, c.KeyVersion, c.Status, c.LastValidatedAt, c.LastSuccessAt, c.LastFailureAt, c.LastErrorClass)
	return err
}
func (r *ServerRepository) GetConnection(ctx context.Context, guildID int64) (*NitradoConnection, error) {
	var c NitradoConnection
	err := r.pool.QueryRow(ctx, `SELECT id,guild_id,credential_ciphertext,credential_nonce,credential_key_version,status,last_validated_at,last_success_at,last_failure_at,COALESCE(last_error_class,'') FROM nitrado_connections WHERE guild_id=$1`, guildID).Scan(&c.ID, &c.GuildID, &c.Ciphertext, &c.Nonce, &c.KeyVersion, &c.Status, &c.LastValidatedAt, &c.LastSuccessAt, &c.LastFailureAt, &c.LastErrorClass)
	return &c, err
}
func (r *ServerRepository) EnsureConfig(ctx context.Context, serverID int64) (*ServerConfig, error) {
	var c ServerConfig
	err := r.pool.QueryRow(ctx, `INSERT INTO server_configs(server_id) VALUES($1) ON CONFLICT(server_id) DO UPDATE SET updated_at=NOW() RETURNING server_id,adm_poll_interval_ms,directory_rescan_interval_seconds,startup_mode,publish_pvp_kills,publish_suicides,publish_unknown_deaths,online_counter_enabled,killfeed_enabled,analytics_enabled,COALESCE(adm_monitor_message_id,'')`, serverID).Scan(&c.ServerID, &c.PollIntervalMS, &c.RescanSeconds, &c.StartupMode, &c.PublishPvPKills, &c.PublishSuicides, &c.PublishUnknownDeaths, &c.OnlineCounterEnabled, &c.KillfeedEnabled, &c.AnalyticsEnabled, &c.ADMMonitorMessageID)
	if err != nil {
		return nil, fmt.Errorf("ensure server config: %w", err)
	}
	return &c, nil
}

// SetADMMonitorMessage persists the ADM monitor's per-server status message ID
// so restarts edit the existing message instead of creating a new one.
func (r *ServerRepository) SetADMMonitorMessage(ctx context.Context, serverID int64, messageID string) error {
	_, err := r.pool.Exec(ctx, `UPDATE server_configs SET adm_monitor_message_id=$2,updated_at=NOW() WHERE server_id=$1`, serverID, messageID)
	return err
}
