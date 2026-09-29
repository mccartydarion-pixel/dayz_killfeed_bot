package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// FeedCard is one row of the immediate-mode feed journal (migration 0057,
// discord_feed_cards). Embed is the card's JSON exactly as it is sent to
// Discord. MessageID is empty until Discord confirms the post.
type FeedCard struct {
	Nonce      string
	Embed      []byte
	DetectedAt time.Time
	EnqueuedAt time.Time
	ChannelID  string
	MessageID  string
	PostedAt   time.Time
}

// FeedCardRepository persists the feed journal. Every write is keyed by
// (feed_key, nonce) or (feed_key, message_id), so it only ever touches the
// calling feed's own cards.
type FeedCardRepository struct{ pool *pgxpool.Pool }

func NewFeedCardRepository(pool *pgxpool.Pool) *FeedCardRepository {
	return &FeedCardRepository{pool: pool}
}

// Record journals queued cards. Re-recording a nonce is a no-op.
func (r *FeedCardRepository) Record(ctx context.Context, feedKey string, cards []FeedCard) error {
	for _, c := range cards {
		if _, err := r.pool.Exec(ctx, `
INSERT INTO discord_feed_cards(feed_key, nonce, embed, detected_at, enqueued_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (feed_key, nonce) DO NOTHING`,
			feedKey, c.Nonce, c.Embed, nullTime(c.DetectedAt), c.EnqueuedAt); err != nil {
			return err
		}
	}
	return nil
}

// MarkPosted records Discord's confirmation of a card.
func (r *FeedCardRepository) MarkPosted(ctx context.Context, feedKey, nonce, channelID, messageID string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `
UPDATE discord_feed_cards SET channel_id=$3, message_id=$4, posted_at=$5
WHERE feed_key=$1 AND nonce=$2`, feedKey, nonce, channelID, messageID, at)
	return err
}

// MarkRemoved records that cards left the channel (deleted, already gone, or
// cleanup abandoned after bounded retries).
func (r *FeedCardRepository) MarkRemoved(ctx context.Context, feedKey string, messageIDs []string, at time.Time) error {
	if len(messageIDs) == 0 {
		return nil
	}
	_, err := r.pool.Exec(ctx, `
UPDATE discord_feed_cards SET removed_at=$3
WHERE feed_key=$1 AND message_id = ANY($2) AND removed_at IS NULL`, feedKey, messageIDs, at)
	return err
}

// MarkDropped records cards that will never be posted, with the reason.
func (r *FeedCardRepository) MarkDropped(ctx context.Context, feedKey string, nonces []string, reason string, at time.Time) error {
	if len(nonces) == 0 {
		return nil
	}
	_, err := r.pool.Exec(ctx, `
UPDATE discord_feed_cards SET dropped_at=$3, drop_reason=$4
WHERE feed_key=$1 AND nonce = ANY($2) AND dropped_at IS NULL AND removed_at IS NULL`, feedKey, nonces, at, reason)
	return err
}

// Open returns the feed's cards that are still queued or still shown, in
// queue order.
func (r *FeedCardRepository) Open(ctx context.Context, feedKey string) ([]FeedCard, error) {
	rows, err := r.pool.Query(ctx, `
SELECT nonce, embed, detected_at, enqueued_at, COALESCE(channel_id, ''), COALESCE(message_id, ''), posted_at
FROM discord_feed_cards
WHERE feed_key=$1 AND removed_at IS NULL AND dropped_at IS NULL
ORDER BY id`, feedKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeedCard
	for rows.Next() {
		var c FeedCard
		var detected, posted *time.Time
		if err := rows.Scan(&c.Nonce, &c.Embed, &detected, &c.EnqueuedAt, &c.ChannelID, &c.MessageID, &posted); err != nil {
			return nil, err
		}
		if detected != nil {
			c.DetectedAt = *detected
		}
		if posted != nil {
			c.PostedAt = *posted
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Purge deletes closed cards (removed or dropped) enqueued before cutoff.
func (r *FeedCardRepository) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
DELETE FROM discord_feed_cards
WHERE enqueued_at < $1 AND (removed_at IS NOT NULL OR dropped_at IS NOT NULL)`, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
