package discord

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// RoutePanelMessage is one recorded panel: message MessageID lives in ChannelID.
type RoutePanelMessage struct{ ChannelID, MessageID string }

// RoutePanelStore durably records which message holds a routed panel in which
// channel (implemented by repository.GuildRoutePanelRepository via an adapter).
type RoutePanelStore interface {
	List(ctx context.Context, guildRowID int64, routeKey string) ([]RoutePanelMessage, error)
	Upsert(ctx context.Context, guildRowID int64, routeKey, channelID, messageID string) error
	Delete(ctx context.Context, guildRowID int64, routeKey, channelID string) error
}

// RoutePanelAPI is the Discord surface RoutePanels needs; *SessionAPI satisfies it.
type RoutePanelAPI interface {
	ChannelMessageSendComplex(channelID string, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) (*discordgo.Message, error)
	ChannelMessageEditComplex(channelID, messageID string, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) (*discordgo.Message, error)
	ChannelMessage(channelID, messageID string) (*discordgo.Message, error)
	ChannelMessageDelete(channelID, messageID string) error
}

// PanelContent is what a routed panel message shows.
type PanelContent struct {
	Embed      *discordgo.MessageEmbed
	Components []discordgo.MessageComponent
}

// RoutePanels reconciles guild-level persistent panels onto routed channels:
// exactly one message per (guild, route key, channel), recorded durably so a
// restart or a route change can never produce a second live copy.
type RoutePanels struct {
	api   RoutePanelAPI
	store RoutePanelStore
	mu    sync.Mutex

	// lastHash remembers the content last written per (guild, route key, channel)
	// so SyncEach can skip an edit that would change nothing. In-memory only: after
	// a restart the first pass re-edits once, which is harmless.
	lastHash map[string]string
}

func NewRoutePanels(api RoutePanelAPI, store RoutePanelStore) *RoutePanels {
	return &RoutePanels{api: api, store: store, lastHash: make(map[string]string)}
}

// ChannelContent renders the panel for one routed channel. An error skips that
// channel for this pass (its existing message is left exactly as it is).
type ChannelContent func(channelID string) (PanelContent, error)

// PanelSyncResult reports what one Sync did.
type PanelSyncResult struct {
	Created, Updated, Removed int
	Errors                    int
}

// Sync makes the panel for (guildRowID, routeKey) exist in every desired
// channel and, when cleanup is true, retires panels recorded for channels no
// longer desired. update=true re-edits existing messages with content (the
// dynamic leaderboard); update=false only verifies they still exist (static
// button panels) and leaves them untouched. A message is recreated only when
// Discord says it is gone - never on a transient error.
func (p *RoutePanels) Sync(ctx context.Context, guildRowID int64, routeKey string, desired []string, content PanelContent, update, cleanup bool) (PanelSyncResult, error) {
	return p.sync(ctx, guildRowID, routeKey, desired, func(string) (PanelContent, error) { return content, nil }, update, cleanup, false)
}

// SyncEach is Sync for panels whose content differs per channel (the bounty board
// shows each channel's own servers). It always updates existing messages, but skips
// an edit whose content is identical to what it last wrote, so a periodic
// reconcile does not touch Discord unless something changed.
func (p *RoutePanels) SyncEach(ctx context.Context, guildRowID int64, routeKey string, desired []string, contentFor ChannelContent, cleanup bool) (PanelSyncResult, error) {
	return p.sync(ctx, guildRowID, routeKey, desired, contentFor, true, cleanup, true)
}

func (p *RoutePanels) sync(ctx context.Context, guildRowID int64, routeKey string, desired []string, contentFor ChannelContent, update, cleanup, skipUnchanged bool) (PanelSyncResult, error) {
	var res PanelSyncResult
	if p == nil || p.api == nil || p.store == nil {
		return res, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	existing, err := p.store.List(ctx, guildRowID, routeKey)
	if err != nil {
		return res, err
	}
	recorded := make(map[string]string, len(existing))
	for _, e := range existing {
		recorded[e.ChannelID] = e.MessageID
	}
	hashKey := func(channelID string) string { return fmt.Sprintf("%d|%s|%s", guildRowID, routeKey, channelID) }

	want := make(map[string]bool, len(desired))
	for _, channelID := range desired {
		want[channelID] = true
		content, contentErr := contentFor(channelID)
		if contentErr != nil {
			res.Errors++
			slog.Warn("component=discord", "event", "route_panel_content_failed", "route_key", routeKey, "guild_id", guildRowID, "channel_id", channelID, "err", contentErr.Error())
			continue
		}
		hash := ""
		if skipUnchanged {
			hash = hashEmbed(content.Embed)
		}
		messageID, has := recorded[channelID]
		if has {
			if update {
				if skipUnchanged && p.lastHash[hashKey(channelID)] == hash {
					continue // nothing changed since the last write
				}
				_, editErr := p.api.ChannelMessageEditComplex(channelID, messageID, content.Embed, content.Components)
				switch {
				case editErr == nil:
					res.Updated++
					if skipUnchanged {
						p.lastHash[hashKey(channelID)] = hash
					}
					continue
				case !isUnknownMessage(editErr):
					res.Errors++
					slog.Warn("component=discord", "event", "route_panel_update_failed", "route_key", routeKey, "guild_id", guildRowID, "channel_id", channelID, "err", editErr.Error())
					continue
				}
			} else {
				_, getErr := p.api.ChannelMessage(channelID, messageID)
				switch {
				case getErr == nil:
					continue
				case !isUnknownMessage(getErr):
					res.Errors++
					slog.Warn("component=discord", "event", "route_panel_verify_failed", "route_key", routeKey, "guild_id", guildRowID, "channel_id", channelID, "err", getErr.Error())
					continue
				}
			}
			// The recorded message is gone: fall through and post a fresh one.
		}
		msg, sendErr := p.api.ChannelMessageSendComplex(channelID, content.Embed, content.Components)
		if sendErr != nil || msg == nil {
			res.Errors++
			if sendErr != nil {
				slog.Warn("component=discord", "event", "route_panel_send_failed", "route_key", routeKey, "guild_id", guildRowID, "channel_id", channelID, "err", sendErr.Error())
			}
			continue
		}
		if upErr := p.store.Upsert(ctx, guildRowID, routeKey, channelID, msg.ID); upErr != nil {
			// Not recorded: retire the message just posted, or the next sync
			// would post another copy next to it.
			_ = p.api.ChannelMessageDelete(channelID, msg.ID)
			res.Errors++
			slog.Warn("component=discord", "event", "route_panel_record_failed", "route_key", routeKey, "guild_id", guildRowID, "channel_id", channelID, "err", upErr.Error())
			continue
		}
		if skipUnchanged {
			p.lastHash[hashKey(channelID)] = hash
		}
		res.Created++
	}

	if cleanup {
		for _, e := range existing {
			if want[e.ChannelID] {
				continue
			}
			// Best-effort: the message may already be gone or the channel deleted.
			if delErr := p.api.ChannelMessageDelete(e.ChannelID, e.MessageID); delErr != nil && !isUnknownMessage(delErr) {
				slog.Warn("component=discord", "event", "route_panel_retire_failed", "route_key", routeKey, "guild_id", guildRowID, "channel_id", e.ChannelID, "err", delErr.Error())
			}
			if delErr := p.store.Delete(ctx, guildRowID, routeKey, e.ChannelID); delErr != nil {
				res.Errors++
				continue
			}
			delete(p.lastHash, hashKey(e.ChannelID))
			res.Removed++
		}
	}
	return res, nil
}

// routePanelRepo is the persistence surface RoutePanelStore adapts.
type routePanelRepo interface {
	List(ctx context.Context, guildRowID int64, routeKey string) ([]repository.GuildRoutePanel, error)
	Upsert(ctx context.Context, guildRowID int64, routeKey, channelID, messageID string) error
	Delete(ctx context.Context, guildRowID int64, routeKey, channelID string) error
}

type routePanelStoreAdapter struct{ repo routePanelRepo }

// NewRoutePanelStore adapts repository.GuildRoutePanelRepository to RoutePanelStore.
func NewRoutePanelStore(repo routePanelRepo) RoutePanelStore { return routePanelStoreAdapter{repo: repo} }

func (a routePanelStoreAdapter) List(ctx context.Context, guildRowID int64, routeKey string) ([]RoutePanelMessage, error) {
	rows, err := a.repo.List(ctx, guildRowID, routeKey)
	if err != nil {
		return nil, err
	}
	out := make([]RoutePanelMessage, 0, len(rows))
	for _, r := range rows {
		out = append(out, RoutePanelMessage{ChannelID: r.ChannelID, MessageID: r.MessageID})
	}
	return out, nil
}

func (a routePanelStoreAdapter) Upsert(ctx context.Context, guildRowID int64, routeKey, channelID, messageID string) error {
	return a.repo.Upsert(ctx, guildRowID, routeKey, channelID, messageID)
}

func (a routePanelStoreAdapter) Delete(ctx context.Context, guildRowID int64, routeKey, channelID string) error {
	return a.repo.Delete(ctx, guildRowID, routeKey, channelID)
}
