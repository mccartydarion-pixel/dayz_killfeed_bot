package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The Discord side of the buyer confirmation (migration 0088, docs/SHOP_ORDER_CONFIRMATION.md
// "Discord"): the delivered-order DM, the ticket channel and the per-server role setup. Work is
// claimed with a lease (FOR UPDATE SKIP LOCKED plus a claimed-at time) so several bot instances
// never send the same DM or create the same channel at once; a crashed claim is retried after the
// lease, up to a fixed number of attempts.

// Notice delivery states.
const (
	NoticePending     = "PENDING"
	NoticeSent        = "SENT"
	NoticeUnavailable = "UNAVAILABLE"
)

const (
	// ShopNoticeMaxAttempts and ShopTicketChannelMaxAttempts bound the retries of one DM / channel.
	ShopNoticeMaxAttempts        = 5
	ShopTicketChannelMaxAttempts = 5
	// shopDiscordLease is how long a claim is held before another instance may retry it.
	shopDiscordLease = 5 * time.Minute
)

// execShop runs a statement that returns no rows.
func execShop(ctx context.Context, q shopQuerier, sql string, args ...any) error {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	rows.Close()
	return rows.Err()
}

// ShopOrderNotice is one delivered order whose buyer should be asked "received or issue?".
type ShopOrderNotice struct {
	PurchaseID, OrganizationID, InstallationID int64
	GuildRowID                                 int64
	DiscordGuildID, GuildName                  string
	// BuyerDiscordID is the Discord account with a VERIFIED link to the buyer's player in this
	// server; "" when there is none (the buyer answers on the website).
	BuyerDiscordID string
	TotalPoints    int64
	DeadlineAt     time.Time
	Attempts       int
	Items          []ShopNoticeItem
}

// ShopNoticeItem is one line of the order, as snapshotted at purchase time.
type ShopNoticeItem struct {
	Name     string
	Quantity int
}

// ClaimPendingNotices leases up to limit undelivered notices of orders still awaiting their buyer.
func (r *ShopConfirmationRepository) ClaimPendingNotices(ctx context.Context, now time.Time, limit int) ([]ShopOrderNotice, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.pool.Query(ctx, `
WITH claimed AS (
    UPDATE shop_order_confirmations c
       SET notify_attempts = c.notify_attempts + 1, notify_claimed_at = $1, updated_at = NOW()
     WHERE c.purchase_id IN (SELECT purchase_id FROM shop_order_confirmations
                              WHERE notify_state = 'PENDING' AND state = 'AWAITING_BUYER'
                                AND (notify_claimed_at IS NULL OR notify_claimed_at <= $2)
                              ORDER BY purchase_id LIMIT $3 FOR UPDATE SKIP LOCKED)
       AND c.notify_state = 'PENDING' AND c.state = 'AWAITING_BUYER'
    RETURNING c.purchase_id, c.organization_id, c.installation_id, c.player_id, c.deadline_at, c.notify_attempts
)
SELECT cl.purchase_id, cl.organization_id, cl.installation_id, g.id, g.discord_guild_id, COALESCE(dc.guild_name, ''), COALESCE(pl.discord_user_id, ''),
       sp.total_points, cl.deadline_at, cl.notify_attempts
  FROM claimed cl
  JOIN shop_purchases sp ON sp.id = cl.purchase_id
  JOIN installations i ON i.id = cl.installation_id
  JOIN discord_guild_connections dc ON dc.id = i.discord_guild_connection_id
  JOIN guilds g ON g.id = dc.guild_id
  LEFT JOIN player_links pl ON pl.guild_id = g.id AND pl.player_id = cl.player_id AND pl.status = 'VERIFIED'
 ORDER BY cl.purchase_id`, now, now.Add(-shopDiscordLease), limit)
	if err != nil {
		return nil, err
	}
	var out []ShopOrderNotice
	for rows.Next() {
		var n ShopOrderNotice
		if err := rows.Scan(&n.PurchaseID, &n.OrganizationID, &n.InstallationID, &n.GuildRowID, &n.DiscordGuildID, &n.GuildName, &n.BuyerDiscordID,
			&n.TotalPoints, &n.DeadlineAt, &n.Attempts); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		items, err := r.noticeItems(ctx, out[i].PurchaseID)
		if err != nil {
			return nil, err
		}
		out[i].Items = items
	}
	return out, nil
}

func (r *ShopConfirmationRepository) noticeItems(ctx context.Context, purchaseID int64) ([]ShopNoticeItem, error) {
	rows, err := r.pool.Query(ctx, `SELECT product_name, quantity FROM shop_purchase_items WHERE purchase_id = $1 ORDER BY id`, purchaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShopNoticeItem
	for rows.Next() {
		var it ShopNoticeItem
		if err := rows.Scan(&it.Name, &it.Quantity); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkNotice ends a notice as SENT (with the DM it was delivered in) or UNAVAILABLE. It only moves
// a PENDING notice, so a late result never overwrites an earlier one.
func (r *ShopConfirmationRepository) MarkNotice(ctx context.Context, purchaseID int64, state, dmChannelID, dmMessageID string) error {
	if state != NoticeSent && state != NoticeUnavailable {
		return errors.New("shop: notice can only end as SENT or UNAVAILABLE")
	}
	return execShop(ctx, r.pool, `UPDATE shop_order_confirmations
   SET notify_state = $2, notified_at = NOW(), dm_channel_id = NULLIF($3, ''), dm_message_id = NULLIF($4, ''), updated_at = NOW()
 WHERE purchase_id = $1 AND notify_state = 'PENDING'`, purchaseID, state, dmChannelID, dmMessageID)
}

// ReleaseNotice gives a claimed notice back for a later retry (a transient Discord failure). Once
// the attempts are used up the notice ends as UNAVAILABLE instead.
func (r *ShopConfirmationRepository) ReleaseNotice(ctx context.Context, purchaseID int64) error {
	return execShop(ctx, r.pool, `UPDATE shop_order_confirmations
   SET notify_state = CASE WHEN notify_attempts >= $2 THEN 'UNAVAILABLE' ELSE notify_state END,
       notified_at = CASE WHEN notify_attempts >= $2 THEN NOW() ELSE notified_at END, updated_at = NOW()
 WHERE purchase_id = $1 AND notify_state = 'PENDING'`, purchaseID, ShopNoticeMaxAttempts)
}

// ShopDiscordOrder is what a Discord button press resolves to: the tenant scope of the order and
// the confirmation itself.
type ShopDiscordOrder struct {
	Confirmation   ShopOrderConfirmation
	GuildRowID     int64
	DiscordGuildID string
	ServerID       int64
	Status         string // the installation's status
}

// OrderForDiscordBuyer resolves a purchase for the Discord account pressing a button. It is found
// only when that account has a VERIFIED link to the order's player in the order's own server, so
// a forged or forwarded button id reaches nothing.
func (r *ShopConfirmationRepository) OrderForDiscordBuyer(ctx context.Context, purchaseID int64, discordUserID string) (ShopDiscordOrder, error) {
	var o ShopDiscordOrder
	var server *int64
	c := &o.Confirmation
	err := r.pool.QueryRow(ctx, `
SELECT c.purchase_id, c.organization_id, c.installation_id, c.player_id, c.state, c.delivered_at, c.deadline_at, c.responded_at, c.response_source,
       g.id, g.discord_guild_id, i.game_server_id, i.status
  FROM shop_order_confirmations c
  JOIN installations i ON i.id = c.installation_id AND i.organization_id = c.organization_id
  JOIN discord_guild_connections dc ON dc.id = i.discord_guild_connection_id
  JOIN guilds g ON g.id = dc.guild_id
  JOIN player_links pl ON pl.guild_id = g.id AND pl.player_id = c.player_id AND pl.status = 'VERIFIED'
 WHERE c.purchase_id = $1 AND pl.discord_user_id = $2`, purchaseID, discordUserID).Scan(
		&c.PurchaseID, &c.OrganizationID, &c.InstallationID, &c.PlayerID, &c.State, &c.DeliveredAt, &c.DeadlineAt, &c.RespondedAt, &c.ResponseSource,
		&o.GuildRowID, &o.DiscordGuildID, &server, &o.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ErrShopConfirmationNotFound
	}
	if server != nil {
		o.ServerID = *server
	}
	return o, err
}

// ShopTicketChannelJob is one OPEN ticket that still needs its private Discord channel.
type ShopTicketChannelJob struct {
	Ticket         ShopOrderTicket
	GuildRowID     int64
	DiscordGuildID string
	Attempts       int
	Items          []ShopNoticeItem
}

// ClaimTicketsNeedingChannel leases up to limit OPEN tickets without a channel. ticketID > 0 limits
// the claim to that ticket (the immediate attempt right after it was opened).
func (r *ShopConfirmationRepository) ClaimTicketsNeedingChannel(ctx context.Context, now time.Time, ticketID int64, limit int) ([]ShopTicketChannelJob, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := r.pool.Query(ctx, `
WITH claimed AS (
    UPDATE shop_order_tickets t
       SET channel_attempts = t.channel_attempts + 1, channel_claimed_at = $1, updated_at = NOW()
     WHERE t.id IN (SELECT id FROM shop_order_tickets
                     WHERE status = 'OPEN' AND discord_channel_id IS NULL AND channel_attempts < $4
                       AND ($5::bigint = 0 OR id = $5)
                       AND (channel_claimed_at IS NULL OR channel_claimed_at <= $2)
                     ORDER BY id LIMIT $3 FOR UPDATE SKIP LOCKED)
       AND t.status = 'OPEN' AND t.discord_channel_id IS NULL
    RETURNING t.id, t.organization_id, t.installation_id, t.purchase_id, t.player_id, t.opened_by_discord_id, t.opened_via, t.reason, t.status,
              t.resolution, t.resolution_note, t.discord_channel_id, t.opened_at, t.resolved_at, t.resolved_by_user_id, t.channel_attempts
)
SELECT cl.*, g.id, g.discord_guild_id
  FROM claimed cl
  JOIN installations i ON i.id = cl.installation_id
  JOIN discord_guild_connections dc ON dc.id = i.discord_guild_connection_id
  JOIN guilds g ON g.id = dc.guild_id
 ORDER BY cl.id`, now, now.Add(-shopDiscordLease), limit, ShopTicketChannelMaxAttempts, ticketID)
	if err != nil {
		return nil, err
	}
	var out []ShopTicketChannelJob
	for rows.Next() {
		var j ShopTicketChannelJob
		t := &j.Ticket
		if err := rows.Scan(&t.ID, &t.OrganizationID, &t.InstallationID, &t.PurchaseID, &t.PlayerID, &t.OpenedByDiscordID, &t.OpenedVia, &t.Reason, &t.Status,
			&t.Resolution, &t.ResolutionNote, &t.DiscordChannelID, &t.OpenedAt, &t.ResolvedAt, &t.ResolvedByUserID, &j.Attempts,
			&j.GuildRowID, &j.DiscordGuildID); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		items, err := r.noticeItems(ctx, out[i].Ticket.PurchaseID)
		if err != nil {
			return nil, err
		}
		out[i].Items = items
	}
	return out, nil
}

// ReleaseTicketChannel ends the lease of a ticket whose channel could not be created, so the next
// sweep may retry it (until the attempts run out).
func (r *ShopConfirmationRepository) ReleaseTicketChannel(ctx context.Context, ticketID int64) error {
	return execShop(ctx, r.pool, `UPDATE shop_order_tickets SET channel_claimed_at = NULL, updated_at = NOW() WHERE id = $1 AND discord_channel_id IS NULL`, ticketID)
}

// ShopTicketDiscordSetup is the Owner role, Staff role and Tickets category of one Discord server
// ("" = not created yet).
type ShopTicketDiscordSetup struct {
	OwnerRoleID, StaffRoleID, CategoryID string
}

// TicketDiscordSetup reads a server's ticket setup (zero value when it has none yet).
func (r *ShopConfirmationRepository) TicketDiscordSetup(ctx context.Context, guildRowID int64) (ShopTicketDiscordSetup, error) {
	var s ShopTicketDiscordSetup
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(owner_role_id, ''), COALESCE(staff_role_id, ''), COALESCE(category_id, '')
  FROM shop_ticket_discord_setup WHERE guild_id = $1`, guildRowID).Scan(&s.OwnerRoleID, &s.StaffRoleID, &s.CategoryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ShopTicketDiscordSetup{}, nil
	}
	return s, err
}

// SaveTicketDiscordSetup stores a server's ticket setup.
func (r *ShopConfirmationRepository) SaveTicketDiscordSetup(ctx context.Context, guildRowID int64, s ShopTicketDiscordSetup) error {
	return execShop(ctx, r.pool, `INSERT INTO shop_ticket_discord_setup(guild_id, owner_role_id, staff_role_id, category_id)
VALUES($1, NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''))
ON CONFLICT (guild_id) DO UPDATE SET owner_role_id = EXCLUDED.owner_role_id, staff_role_id = EXCLUDED.staff_role_id,
    category_id = EXCLUDED.category_id, updated_at = NOW()`, guildRowID, s.OwnerRoleID, s.StaffRoleID, s.CategoryID)
}
