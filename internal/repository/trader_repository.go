package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TraderRepository stores the Discord Trader of each installation (docs/TRADER.md).
type TraderRepository struct{ pool *pgxpool.Pool }

func NewTraderRepository(pool *pgxpool.Pool) *TraderRepository {
	return &TraderRepository{pool: pool}
}

// TraderHub is an installation's Trader: what /trader setup made in its Discord server, and
// whether the trader is open.
type TraderHub struct {
	InstallationID, OrganizationID  int64
	DiscordGuildID                  string
	CategoryID, VoiceChannelID      string
	TraderChannelID, DealsChannelID string
	PanelChannelID, OrdersChannelID string
	TraderRoleID                    string
	PostMessageID, PanelMessageID   string
	Open                            bool
	// VoiceNameApplied is the status name the voice channel was last renamed to.
	VoiceNameApplied       string
	OpenChangedAt          *time.Time
	OpenChangedByDiscordID string
	// DealsRoleID is the opt-in role pinged for new deals and sales.
	DealsRoleID string
	// ClosedNote is what players see while the trader is closed ("" for none).
	ClosedNote string
	// Opening hours: open at HoursOpen and close at HoursClose (HH:MM in HoursTZ) every day; ""
	// when the trader has none. HoursAppliedAt is the last opening or closing time applied.
	HoursOpen, HoursClose, HoursTZ string
	HoursAppliedAt                 *time.Time
}

const traderHubCols = `installation_id, organization_id, discord_guild_id, category_id, voice_channel_id, trader_channel_id, deals_channel_id,
 panel_channel_id, orders_channel_id, trader_role_id, post_message_id, panel_message_id, is_open, voice_name_applied, open_changed_at, open_changed_by_discord_id,
 deals_role_id, closed_note, hours_open, hours_close, hours_tz, hours_applied_at`

func scanTraderHub(row pgx.Row) (TraderHub, error) {
	var h TraderHub
	err := row.Scan(&h.InstallationID, &h.OrganizationID, &h.DiscordGuildID, &h.CategoryID, &h.VoiceChannelID, &h.TraderChannelID, &h.DealsChannelID,
		&h.PanelChannelID, &h.OrdersChannelID, &h.TraderRoleID, &h.PostMessageID, &h.PanelMessageID, &h.Open, &h.VoiceNameApplied, &h.OpenChangedAt, &h.OpenChangedByDiscordID,
		&h.DealsRoleID, &h.ClosedNote, &h.HoursOpen, &h.HoursClose, &h.HoursTZ, &h.HoursAppliedAt)
	return h, err
}

// Get returns installationID's Trader, nil when it has none.
func (r *TraderRepository) Get(ctx context.Context, installationID int64) (*TraderHub, error) {
	h, err := scanTraderHub(r.pool.QueryRow(ctx, `SELECT `+traderHubCols+` FROM trader_hubs WHERE installation_id=$1`, installationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// List returns every Trader.
func (r *TraderRepository) List(ctx context.Context) ([]TraderHub, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+traderHubCols+` FROM trader_hubs ORDER BY installation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TraderHub
	for rows.Next() {
		h, err := scanTraderHub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Save stores what /trader setup made. A new Trader starts open; saving again keeps the open state.
func (r *TraderRepository) Save(ctx context.Context, h TraderHub) (*TraderHub, error) {
	out, err := scanTraderHub(r.pool.QueryRow(ctx, `INSERT INTO trader_hubs(installation_id, organization_id, discord_guild_id, category_id, voice_channel_id,
 trader_channel_id, deals_channel_id, panel_channel_id, orders_channel_id, trader_role_id, post_message_id, panel_message_id, deals_role_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT (installation_id) DO UPDATE SET organization_id=EXCLUDED.organization_id, discord_guild_id=EXCLUDED.discord_guild_id,
 category_id=EXCLUDED.category_id, voice_channel_id=EXCLUDED.voice_channel_id, trader_channel_id=EXCLUDED.trader_channel_id,
 deals_channel_id=EXCLUDED.deals_channel_id, panel_channel_id=EXCLUDED.panel_channel_id, orders_channel_id=EXCLUDED.orders_channel_id,
 trader_role_id=EXCLUDED.trader_role_id, post_message_id=EXCLUDED.post_message_id, panel_message_id=EXCLUDED.panel_message_id,
 deals_role_id=EXCLUDED.deals_role_id,
 voice_name_applied=CASE WHEN trader_hubs.voice_channel_id=EXCLUDED.voice_channel_id THEN trader_hubs.voice_name_applied ELSE '' END,
 updated_at=NOW()
RETURNING `+traderHubCols,
		h.InstallationID, h.OrganizationID, h.DiscordGuildID, h.CategoryID, h.VoiceChannelID, h.TraderChannelID, h.DealsChannelID,
		h.PanelChannelID, h.OrdersChannelID, h.TraderRoleID, h.PostMessageID, h.PanelMessageID, h.DealsRoleID))
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SetOpen opens or closes installationID's Trader; note is what players see while it is closed
// (opening clears it). changed is false when it already was (a closed trader's note is still
// updated).
func (r *TraderRepository) SetOpen(ctx context.Context, installationID int64, open bool, byDiscordID, note string) (hub *TraderHub, changed bool, err error) {
	if open {
		note = ""
	}
	h, err := scanTraderHub(r.pool.QueryRow(ctx, `UPDATE trader_hubs SET is_open=$2, open_changed_at=NOW(), open_changed_by_discord_id=$3, closed_note=$4, updated_at=NOW()
WHERE installation_id=$1 AND is_open<>$2 RETURNING `+traderHubCols, installationID, open, byDiscordID, note))
	if errors.Is(err, pgx.ErrNoRows) && !open {
		if _, err := r.pool.Exec(ctx, `UPDATE trader_hubs SET closed_note=$2, updated_at=NOW() WHERE installation_id=$1 AND NOT is_open`, installationID, note); err != nil {
			return nil, false, err
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		current, getErr := r.Get(ctx, installationID)
		return current, false, getErr
	}
	if err != nil {
		return nil, false, err
	}
	return &h, true, nil
}

// SetMessages records the trader post and panel messages.
func (r *TraderRepository) SetMessages(ctx context.Context, installationID int64, postMessageID, panelMessageID string) error {
	_, err := r.pool.Exec(ctx, `UPDATE trader_hubs SET post_message_id=$2, panel_message_id=$3, updated_at=NOW() WHERE installation_id=$1`,
		installationID, postMessageID, panelMessageID)
	return err
}

// SetVoiceApplied records the name the voice channel was renamed to.
func (r *TraderRepository) SetVoiceApplied(ctx context.Context, installationID int64, name string) error {
	_, err := r.pool.Exec(ctx, `UPDATE trader_hubs SET voice_name_applied=$2, updated_at=NOW() WHERE installation_id=$1`, installationID, name)
	return err
}

// OrdersWaiting counts installationID's shop orders that are paid and waiting to be handed over.
func (r *TraderRepository) OrdersWaiting(ctx context.Context, installationID int64) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM shop_purchases WHERE installation_id=$1 AND status='PENDING_FULFILLMENT'`, installationID).Scan(&n)
	return n, err
}

// SetHours stores installationID's opening hours ("" for none), with appliedAt as the last opening
// or closing time already applied.
func (r *TraderRepository) SetHours(ctx context.Context, installationID int64, open, close, tz string, appliedAt *time.Time) (*TraderHub, error) {
	h, err := scanTraderHub(r.pool.QueryRow(ctx, `UPDATE trader_hubs SET hours_open=$2, hours_close=$3, hours_tz=$4, hours_applied_at=$5, updated_at=NOW()
WHERE installation_id=$1 RETURNING `+traderHubCols, installationID, open, close, tz, appliedAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// ApplyHours opens or closes installationID's Trader for the opening or closing time at, once:
// applied is false when that time (or a later one) was already applied, or the trader has no
// hours. changed is whether the open state changed.
func (r *TraderRepository) ApplyHours(ctx context.Context, installationID int64, open bool, at time.Time) (hub *TraderHub, applied, changed bool, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, false, err
	}
	defer tx.Rollback(ctx)
	var was bool
	err = tx.QueryRow(ctx, `SELECT is_open FROM trader_hubs WHERE installation_id=$1 AND hours_open<>'' AND (hours_applied_at IS NULL OR hours_applied_at<$2)
FOR UPDATE`, installationID, at).Scan(&was)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	changed = was != open
	h, err := scanTraderHub(tx.QueryRow(ctx, `UPDATE trader_hubs SET is_open=$2, hours_applied_at=$3, updated_at=NOW(),
 open_changed_at=CASE WHEN $4 THEN NOW() ELSE open_changed_at END,
 open_changed_by_discord_id=CASE WHEN $4 THEN '' ELSE open_changed_by_discord_id END,
 closed_note=CASE WHEN $4 THEN '' ELSE closed_note END
WHERE installation_id=$1 RETURNING `+traderHubCols, installationID, open, at, changed))
	if err != nil {
		return nil, false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, false, err
	}
	return &h, true, changed, nil
}
