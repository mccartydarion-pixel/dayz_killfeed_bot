package discord

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

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

// PanelContent is what a routed panel message shows. Single-embed panels set
// Embed (unchanged behaviour). A panel that is one message carrying several
// embeds (the Auto Leaderboard) sets Embeds instead; when Embeds is non-empty
// it wins and the whole array is sent/edited in one call.
type PanelContent struct {
	Embed      *discordgo.MessageEmbed
	Embeds     []*discordgo.MessageEmbed
	Components []discordgo.MessageComponent
	// Files are attached to the message (an embed shows one with "attachment://<name>"). An edit
	// replaces the message's previous attachments with these.
	Files []PanelFile
}

// PanelFile is one attachment of a panel. The bytes are kept (not a reader) so a send that follows
// a failed edit can attach them again.
type PanelFile struct {
	Name, ContentType string
	Data              []byte
}

func (c PanelContent) discordFiles() []*discordgo.File {
	out := make([]*discordgo.File, 0, len(c.Files))
	for _, f := range c.Files {
		out = append(out, &discordgo.File{Name: f.Name, ContentType: f.ContentType, Reader: bytes.NewReader(f.Data)})
	}
	return out
}

// panelEmbeds is the content's embeds as one list.
func (c PanelContent) panelEmbeds() []*discordgo.MessageEmbed {
	if len(c.Embeds) > 0 {
		return c.Embeds
	}
	if c.Embed != nil {
		return []*discordgo.MessageEmbed{c.Embed}
	}
	return nil
}

// FileMessageAPI sends and edits a panel message that carries attachments. *SessionAPI implements
// it; on an API without it a panel's Files are left off.
type FileMessageAPI interface {
	ChannelMessageSendWithFiles(channelID string, embeds []*discordgo.MessageEmbed, components []discordgo.MessageComponent, files []*discordgo.File) (*discordgo.Message, error)
	ChannelMessageEditWithFiles(channelID, messageID string, embeds []*discordgo.MessageEmbed, components []discordgo.MessageComponent, files []*discordgo.File) (*discordgo.Message, error)
}

// MultiEmbedMessageAPI sends and edits ONE message carrying several embeds.
// *SessionAPI implements it. It is a separate, optional surface so every
// existing single-embed RoutePanelAPI/MessageEditor keeps working unchanged.
type MultiEmbedMessageAPI interface {
	ChannelMessageSendEmbeds(channelID string, embeds []*discordgo.MessageEmbed, components []discordgo.MessageComponent) (*discordgo.Message, error)
	ChannelMessageEditEmbeds(channelID, messageID string, embeds []*discordgo.MessageEmbed, components []discordgo.MessageComponent) (*discordgo.Message, error)
}

// sendPanel posts content as one message (multi-embed when Embeds is set).
func (p *RoutePanels) sendPanel(channelID string, content PanelContent) (*discordgo.Message, error) {
	if len(content.Files) > 0 {
		if api, ok := p.api.(FileMessageAPI); ok {
			return api.ChannelMessageSendWithFiles(channelID, content.panelEmbeds(), content.Components, content.discordFiles())
		}
	}
	if len(content.Embeds) > 0 {
		api, ok := p.api.(MultiEmbedMessageAPI)
		if !ok {
			return nil, errNoMultiEmbed
		}
		return api.ChannelMessageSendEmbeds(channelID, content.Embeds, content.Components)
	}
	return p.api.ChannelMessageSendComplex(channelID, content.Embed, content.Components)
}

// editPanel replaces an existing panel message's content in one edit.
func (p *RoutePanels) editPanel(channelID, messageID string, content PanelContent) (*discordgo.Message, error) {
	if len(content.Files) > 0 {
		if api, ok := p.api.(FileMessageAPI); ok {
			return api.ChannelMessageEditWithFiles(channelID, messageID, content.panelEmbeds(), content.Components, content.discordFiles())
		}
	}
	if len(content.Embeds) > 0 {
		api, ok := p.api.(MultiEmbedMessageAPI)
		if !ok {
			return nil, errNoMultiEmbed
		}
		return api.ChannelMessageEditEmbeds(channelID, messageID, content.Embeds, content.Components)
	}
	return p.api.ChannelMessageEditComplex(channelID, messageID, content.Embed, content.Components)
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
	// faults records, per (guild, route key, channel), a permanent configuration fault
	// (the channel is gone, or the bot may not see or post in it: 403 / 404). Retrying
	// every sync cannot fix those; the channel is retried every routePanelFaultRetry
	// instead, and onFault is told once per fault transition so the guild's admins hear
	// about it (docs/SAAS_RUNTIME_ROUTING.md).
	faults  map[string]panelFault
	onFault func(guildRowID int64, routeKey, channelID, class string)
	now     func() time.Time
}

// panelFault is one channel's standing configuration fault.
type panelFault struct {
	Class     string
	Since     time.Time
	LastTried time.Time
}

// routePanelFaultRetry is how often a channel with a standing fault is tried again.
const routePanelFaultRetry = 10 * time.Minute

func NewRoutePanels(api RoutePanelAPI, store RoutePanelStore) *RoutePanels {
	return &RoutePanels{api: api, store: store, lastHash: make(map[string]string), faults: make(map[string]panelFault), now: time.Now}
}

// SetFaultHandler registers the callback for a new permanent fault on a routed channel. It
// runs on the syncing goroutine and must not block.
func (p *RoutePanels) SetFaultHandler(fn func(guildRowID int64, routeKey, channelID, class string)) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.onFault = fn
	p.mu.Unlock()
}

// Faults lists the standing channel faults: channels the bot cannot see, post in or that no
// longer exist, keyed by (guild row id, route key, channel id).
func (p *RoutePanels) Faults() []PanelFault {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PanelFault, 0, len(p.faults))
	for key, f := range p.faults {
		var guildRowID int64
		var routeKey, channelID string
		if _, err := fmt.Sscanf(key, "%d|%s", &guildRowID, &routeKey); err != nil {
			continue
		}
		if i := strings.LastIndex(routeKey, "|"); i >= 0 {
			channelID, routeKey = routeKey[i+1:], routeKey[:i]
		}
		out = append(out, PanelFault{GuildRowID: guildRowID, RouteKey: routeKey, ChannelID: channelID, Class: f.Class, Since: f.Since})
	}
	return out
}

// PanelFault is one entry of Faults.
type PanelFault struct {
	GuildRowID int64
	RouteKey   string
	ChannelID  string
	Class      string
	Since      time.Time
}

// faultSkip reports whether a channel with a standing fault is still inside its retry
// backoff. Caller holds p.mu.
func (p *RoutePanels) faultSkip(key string) bool {
	f, ok := p.faults[key]
	if !ok {
		return false
	}
	if p.now().Sub(f.LastTried) < routePanelFaultRetry {
		return true
	}
	f.LastTried = p.now()
	p.faults[key] = f
	return false
}

// noteFault classifies err; a permanent fault is recorded (first time: reported through
// onFault) and true is returned. Caller holds p.mu.
func (p *RoutePanels) noteFault(key string, guildRowID int64, routeKey, channelID string, err error) bool {
	class := permanentCounterFault(err)
	if class == "" {
		return false
	}
	now := p.now()
	if prev, ok := p.faults[key]; ok && prev.Class == class {
		prev.LastTried = now
		p.faults[key] = prev
		return true
	}
	p.faults[key] = panelFault{Class: class, Since: now, LastTried: now}
	slog.Error("component=discord", "event", "route_panel_channel_fault", "route_key", routeKey, "guild_id", guildRowID, "channel_id", channelID, "fault_class", class,
		"msg", "panel syncing for this channel paused; retried every 10 minutes until the channel is repaired")
	if p.onFault != nil {
		go p.onFault(guildRowID, routeKey, channelID, class)
	}
	return true
}

// clearFault forgets a channel's standing fault after a successful Discord call. Caller holds p.mu.
func (p *RoutePanels) clearFault(key string) {
	if _, ok := p.faults[key]; ok {
		delete(p.faults, key)
		slog.Info("component=discord", "event", "route_panel_channel_fault_cleared", "key", key)
	}
}

// Recorded lists the messages currently recorded for (guildRowID, routeKey).
func (p *RoutePanels) Recorded(ctx context.Context, guildRowID int64, routeKey string) ([]RoutePanelMessage, error) {
	if p == nil || p.store == nil {
		return nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.store.List(ctx, guildRowID, routeKey)
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
		if p.faultSkip(hashKey(channelID)) {
			continue // a channel the bot cannot use: retried on its own, slower cadence
		}
		content, contentErr := contentFor(channelID)
		if contentErr != nil {
			res.Errors++
			slog.Warn("component=discord", "event", "route_panel_content_failed", "route_key", routeKey, "guild_id", guildRowID, "channel_id", channelID, "err", contentErr.Error())
			continue
		}
		hash := ""
		if skipUnchanged {
			hash = hashPanelContent(content)
		}
		messageID, has := recorded[channelID]
		if has {
			if update {
				if skipUnchanged && p.lastHash[hashKey(channelID)] == hash {
					continue // nothing changed since the last write
				}
				_, editErr := p.editPanel(channelID, messageID, content)
				switch {
				case editErr == nil:
					res.Updated++
					p.clearFault(hashKey(channelID))
					if skipUnchanged {
						p.lastHash[hashKey(channelID)] = hash
					}
					continue
				case !isUnknownMessage(editErr):
					if !p.noteFault(hashKey(channelID), guildRowID, routeKey, channelID, editErr) {
						res.Errors++
						slog.Warn("component=discord", "event", "route_panel_update_failed", "route_key", routeKey, "guild_id", guildRowID, "channel_id", channelID, "err", editErr.Error())
					}
					continue
				}
			} else {
				_, getErr := p.api.ChannelMessage(channelID, messageID)
				switch {
				case getErr == nil:
					p.clearFault(hashKey(channelID))
					continue
				case !isUnknownMessage(getErr):
					if !p.noteFault(hashKey(channelID), guildRowID, routeKey, channelID, getErr) {
						res.Errors++
						slog.Warn("component=discord", "event", "route_panel_verify_failed", "route_key", routeKey, "guild_id", guildRowID, "channel_id", channelID, "err", getErr.Error())
					}
					continue
				}
			}
			// The recorded message is gone: fall through and post a fresh one.
		}
		msg, sendErr := p.sendPanel(channelID, content)
		if sendErr != nil || msg == nil {
			if sendErr != nil && p.noteFault(hashKey(channelID), guildRowID, routeKey, channelID, sendErr) {
				continue
			}
			res.Errors++
			if sendErr != nil {
				slog.Warn("component=discord", "event", "route_panel_send_failed", "route_key", routeKey, "guild_id", guildRowID, "channel_id", channelID, "err", sendErr.Error())
			}
			continue
		}
		p.clearFault(hashKey(channelID))
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
func NewRoutePanelStore(repo routePanelRepo) RoutePanelStore {
	return routePanelStoreAdapter{repo: repo}
}

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
