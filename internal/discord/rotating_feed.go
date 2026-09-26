package discord

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// rotatingFeedAPI is the narrow Discord surface RotatingFeed needs.
// *discordgo.Session satisfies it directly; tests use a fake.
type rotatingFeedAPI interface {
	ChannelMessagesBulkDelete(channelID string, messages []string, options ...discordgo.RequestOption) error
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// RotatingFeed batches embeds and flushes them to a channel on a fixed
// interval: delete the previous cycle's messages, then post up to maxItems
// of the most recently enqueued embeds. A quiet cycle (nothing enqueued)
// still deletes the previous batch on schedule and posts nothing - the
// channel can sit empty until the next item arrives, a deliberate
// consequence of a fixed-interval cycle rather than a "post once N
// accumulate" one.
type RotatingFeed struct {
	api         rotatingFeedAPI
	store       SetupStore
	guildID     string
	channelIDFn func(*GuildSetup) string
	interval    time.Duration
	maxItems    int

	mu            sync.Mutex
	pending       []*discordgo.MessageEmbed
	lastMsgIDs    []string
	lastChannelID string

	// routeChannelFn, when set, is consulted first each flush (the
	// installation route model - see KillfeedPublisher.RouteChannelID); an
	// empty result falls back to the legacy GuildSetup field below, so
	// guilds without a configured route behave exactly as before.
	routeChannelFn func() string

	done chan struct{}
}

// SetRouteChannelResolver attaches the installation-route lookup that takes
// priority over the legacy GuildSetup channel. Optional.
func (f *RotatingFeed) SetRouteChannelResolver(fn func() string) {
	if f == nil {
		return
	}
	f.routeChannelFn = fn
}

// NewRotatingFeed creates a feed. channelIDFn extracts the relevant channel
// field from GuildSetup (e.g. KillfeedChannelID or DeathChannelID) so the
// same type serves both channels.
func NewRotatingFeed(api rotatingFeedAPI, store SetupStore, guildID string, channelIDFn func(*GuildSetup) string, interval time.Duration, maxItems int) *RotatingFeed {
	return &RotatingFeed{api: api, store: store, guildID: guildID, channelIDFn: channelIDFn, interval: interval, maxItems: maxItems, done: make(chan struct{})}
}

// Enqueue appends an embed to the current cycle's pending batch. Safe to
// call concurrently with Run's ticker goroutine.
func (f *RotatingFeed) Enqueue(embed *discordgo.MessageEmbed) {
	if f == nil || embed == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append(f.pending, embed)
}

// Run ticks flush() on the configured interval until ctx is done, then does
// one final flush before returning so a graceful shutdown/redeploy never
// silently drops whatever was enqueued since the last cycle (up to
// interval's worth of kills/deaths would otherwise vanish - never posted,
// even though they were already persisted to the database). Intended to be
// started as its own goroutine for the lifetime of a server worker.
func (f *RotatingFeed) Run(ctx context.Context) {
	if f == nil {
		return
	}
	defer close(f.done)
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			f.flush()
			return
		case <-ticker.C:
			f.flush()
		}
	}
}

// WaitDone blocks until Run has returned, including its final on-shutdown
// flush. Callers must only call this after Run's ctx is cancelled (or about
// to be) - see App.shutdown, which mirrors the same pattern already used to
// drain PersistenceQueue before the process exits.
func (f *RotatingFeed) WaitDone() {
	if f == nil {
		return
	}
	<-f.done
}

func (f *RotatingFeed) channelID() string {
	if f.routeChannelFn != nil {
		if id := f.routeChannelFn(); id != "" {
			return id
		}
	}
	if f.store == nil || f.guildID == "" {
		return ""
	}
	setup, err := f.store.Get(f.guildID)
	if err != nil || setup == nil {
		return ""
	}
	return f.channelIDFn(setup)
}

// flush deletes the previous batch, then posts up to maxItems of the most
// recently enqueued embeds as the new batch.
func (f *RotatingFeed) flush() {
	if f.api == nil {
		return
	}
	f.mu.Lock()
	items := f.pending
	f.pending = nil
	lastIDs := f.lastMsgIDs
	lastChannel := f.lastChannelID
	f.mu.Unlock()

	if len(lastIDs) > 0 && lastChannel != "" {
		if err := f.api.ChannelMessagesBulkDelete(lastChannel, lastIDs); err != nil {
			slog.Warn("component=discord", "msg", "rotating feed cleanup failed", "channel_id", lastChannel, "err", err.Error())
		}
	}

	channelID := f.channelID()
	if channelID == "" {
		f.mu.Lock()
		f.lastMsgIDs = nil
		f.lastChannelID = ""
		f.mu.Unlock()
		return
	}

	if len(items) > f.maxItems {
		items = items[len(items)-f.maxItems:]
	}

	newIDs := make([]string, 0, len(items))
	var retry []*discordgo.MessageEmbed
	for i, embed := range items {
		msg, err := deliverMessage(f.api, "KILLFEED", channelID, &discordgo.MessageSend{
			Embeds: []*discordgo.MessageEmbed{embed},
			AllowedMentions: &discordgo.MessageAllowedMentions{
				Parse: []discordgo.AllowedMentionType{},
			},
		})
		if err != nil {
			class := ClassifyDeliveryError(err)
			slog.Warn("component=discord", "msg", "rotating feed publish failed", "channel_id", channelID, "class", class, "err", err.Error())
			if class == DeliveryConfigFault {
				// The channel is unusable: stop this cycle instead of
				// failing every remaining item against it, and keep them
				// for the next cycle (a repaired route picks them up).
				retry = append(retry, items[i:]...)
				break
			}
			if class != DeliveryRejected {
				retry = append(retry, embed) // transient after bounded retries: next cycle
			}
			continue
		}
		newIDs = append(newIDs, msg.ID)
	}

	f.mu.Lock()
	f.lastMsgIDs = newIDs
	f.lastChannelID = channelID
	if len(retry) > 0 {
		// Undelivered items go ahead of anything enqueued meanwhile; the
		// next flush still trims to the newest maxItems.
		f.pending = append(retry, f.pending...)
	}
	f.mu.Unlock()
}
