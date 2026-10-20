package repository

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SecurityStorePanelRepository stores the Discord Security Store panel per server.
type SecurityStorePanelRepository struct{ pool *pgxpool.Pool }

func NewSecurityStorePanelRepository(pool *pgxpool.Pool) *SecurityStorePanelRepository {
	return &SecurityStorePanelRepository{pool: pool}
}

var (
	ErrInvalidSecurityPanel = errors.New("invalid security store panel")
	discordSnowflake        = regexp.MustCompile(`^[0-9]{5,25}$`)
)

type SecurityStorePanel struct {
	InstallationID int64      `json:"-"`
	GuildID        int64      `json:"-"`
	ServerID       int64      `json:"-"`
	ChannelID      string     `json:"channelId"`
	MessageID      string     `json:"messageId,omitempty"`
	Enabled        bool       `json:"enabled"`
	LastPostedAt   *time.Time `json:"lastPostedAt,omitempty"`
	LastError      string     `json:"lastError,omitempty"`
}

func (r *SecurityStorePanelRepository) ready() bool { return r != nil && r.pool != nil }

// Get returns the panel for one server, or nil when none was set up.
func (r *SecurityStorePanelRepository) Get(ctx context.Context, s SecurityScope) (*SecurityStorePanel, error) {
	if !r.ready() || !s.valid() {
		return nil, ErrInvalidSecurityPanel
	}
	p := SecurityStorePanel{InstallationID: s.InstallationID, GuildID: s.GuildID, ServerID: s.ServerID}
	err := r.pool.QueryRow(ctx, `SELECT channel_id,message_id,enabled,last_posted_at,last_error FROM security_store_panels
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3`, s.InstallationID, s.GuildID, s.ServerID).
		Scan(&p.ChannelID, &p.MessageID, &p.Enabled, &p.LastPostedAt, &p.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Set chooses the channel and switch. Moving to another channel forgets the
// old message, so a fresh panel is posted there.
func (r *SecurityStorePanelRepository) Set(ctx context.Context, s SecurityScope, channelID string, enabled bool, actorUserID *int64) (SecurityStorePanel, error) {
	if !r.ready() || !s.valid() || !discordSnowflake.MatchString(channelID) {
		return SecurityStorePanel{}, ErrInvalidSecurityPanel
	}
	p := SecurityStorePanel{InstallationID: s.InstallationID, GuildID: s.GuildID, ServerID: s.ServerID}
	err := r.pool.QueryRow(ctx, `INSERT INTO security_store_panels(installation_id,guild_id,server_id,channel_id,enabled,updated_by_user_id,updated_at)
 VALUES ($1,$2,$3,$4,$5,$6,NOW())
 ON CONFLICT (installation_id,server_id) DO UPDATE SET
  message_id=CASE WHEN security_store_panels.channel_id=EXCLUDED.channel_id THEN security_store_panels.message_id ELSE '' END,
  channel_id=EXCLUDED.channel_id,enabled=EXCLUDED.enabled,updated_by_user_id=EXCLUDED.updated_by_user_id,updated_at=NOW(),last_error=''
 WHERE security_store_panels.guild_id=EXCLUDED.guild_id
 RETURNING channel_id,message_id,enabled,last_posted_at,last_error`,
		s.InstallationID, s.GuildID, s.ServerID, channelID, enabled, actorUserID).
		Scan(&p.ChannelID, &p.MessageID, &p.Enabled, &p.LastPostedAt, &p.LastError)
	if err != nil {
		return SecurityStorePanel{}, err
	}
	return p, nil
}

// MarkPosted records the message now showing the panel, or why posting failed.
func (r *SecurityStorePanelRepository) MarkPosted(ctx context.Context, s SecurityScope, channelID, messageID, postErr string) error {
	if !r.ready() || !s.valid() || (messageID != "" && !discordSnowflake.MatchString(messageID)) {
		return ErrInvalidSecurityPanel
	}
	if r := []rune(postErr); len(r) > 300 {
		postErr = string(r[:300])
	}
	// Only touch the row if the channel is still the one we posted to.
	_, err := r.pool.Exec(ctx, `UPDATE security_store_panels SET
  message_id=CASE WHEN $5='' THEN message_id ELSE $5 END,
  last_posted_at=CASE WHEN $6='' THEN NOW() ELSE last_posted_at END,last_error=$6
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3 AND channel_id=$4`,
		s.InstallationID, s.GuildID, s.ServerID, channelID, messageID, postErr)
	return err
}

// Enabled lists every switched-on panel, for the refresh worker.
func (r *SecurityStorePanelRepository) Enabled(ctx context.Context) ([]SecurityStorePanel, error) {
	if !r.ready() {
		return nil, ErrInvalidSecurityPanel
	}
	rows, err := r.pool.Query(ctx, `SELECT installation_id,guild_id,server_id,channel_id,message_id,enabled,last_posted_at,last_error
 FROM security_store_panels WHERE enabled ORDER BY installation_id,server_id LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecurityStorePanel
	for rows.Next() {
		var p SecurityStorePanel
		if err := rows.Scan(&p.InstallationID, &p.GuildID, &p.ServerID, &p.ChannelID, &p.MessageID, &p.Enabled, &p.LastPostedAt, &p.LastError); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
