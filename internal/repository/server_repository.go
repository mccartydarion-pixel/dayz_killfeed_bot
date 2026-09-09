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
}
type NitradoConnection struct {
	ID, GuildID                                   int64
	Ciphertext, Nonce                             []byte
	KeyVersion                                    int
	Status                                        string
	LastValidatedAt, LastSuccessAt, LastFailureAt *time.Time
	LastErrorClass                                string
}
type ServerConfig struct {
	ServerID                                                                                                        int64
	PollIntervalMS, RescanSeconds                                                                                   int
	StartupMode                                                                                                     string
	PublishPvPKills, PublishSuicides, PublishUnknownDeaths, OnlineCounterEnabled, KillfeedEnabled, AnalyticsEnabled bool
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
func (r *ServerRepository) DefaultServerID(ctx context.Context, guildID int64) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT id FROM game_servers WHERE guild_id=$1 AND active ORDER BY id LIMIT 1`, guildID).Scan(&id)
	return id, err
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
	err := r.pool.QueryRow(ctx, `INSERT INTO server_configs(server_id) VALUES($1) ON CONFLICT(server_id) DO UPDATE SET updated_at=NOW() RETURNING server_id,adm_poll_interval_ms,directory_rescan_interval_seconds,startup_mode,publish_pvp_kills,publish_suicides,publish_unknown_deaths,online_counter_enabled,killfeed_enabled,analytics_enabled`, serverID).Scan(&c.ServerID, &c.PollIntervalMS, &c.RescanSeconds, &c.StartupMode, &c.PublishPvPKills, &c.PublishSuicides, &c.PublishUnknownDeaths, &c.OnlineCounterEnabled, &c.KillfeedEnabled, &c.AnalyticsEnabled)
	if err != nil {
		return nil, fmt.Errorf("ensure server config: %w", err)
	}
	return &c, nil
}
