package discord

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Invite tracking (docs/CLIENT_HUB_GROWTH.md): which Discord invite brought each new member.
// Discord does not say which invite a member used, so the tracker keeps every guild's invite use
// counts and, when a member joins, re-reads them: the one invite whose count went up is the one
// used. Reading invites needs the Manage Server permission; without it every join is recorded as
// unattributed and the Client Hub says why.

// InviteLister is the one Discord call the tracker makes.
type InviteLister interface {
	GuildInvites(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Invite, error)
}

// InviteJoinStore persists joins and leaves.
type InviteJoinStore interface {
	RecordInviteJoin(ctx context.Context, join repository.InviteJoin) error
	RecordInviteLeave(ctx context.Context, guildDiscordID, memberDiscordID string, at time.Time) error
}

type inviteState struct {
	uses, maxUses int
	inviterID     string
	inviterName   string
}

// InviteTracker caches invite counts per guild. Joins are attributed one at a time per guild so
// two members joining together never read the same snapshot.
type InviteTracker struct {
	api   InviteLister
	store InviteJoinStore
	now   func() time.Time

	mu     sync.Mutex
	guilds map[string]*guildInvites
}

type guildInvites struct {
	mu       sync.Mutex
	invites  map[string]inviteState
	ready    bool
	lastErr  string
	lastSync time.Time
}

func NewInviteTracker(api InviteLister, store InviteJoinStore) *InviteTracker {
	return &InviteTracker{api: api, store: store, now: time.Now, guilds: map[string]*guildInvites{}}
}

func (t *InviteTracker) guild(id string) *guildInvites {
	t.mu.Lock()
	defer t.mu.Unlock()
	g := t.guilds[id]
	if g == nil {
		g = &guildInvites{invites: map[string]inviteState{}}
		t.guilds[id] = g
	}
	return g
}

func toState(list []*discordgo.Invite) map[string]inviteState {
	out := make(map[string]inviteState, len(list))
	for _, inv := range list {
		if inv == nil || inv.Code == "" {
			continue
		}
		s := inviteState{uses: inv.Uses, maxUses: inv.MaxUses}
		if inv.Inviter != nil {
			s.inviterID, s.inviterName = inv.Inviter.ID, inv.Inviter.Username
		}
		out[inv.Code] = s
	}
	return out
}

// Snapshot (re)reads a guild's invites. Called when the bot sees the guild.
func (t *InviteTracker) Snapshot(guildID string) {
	if t == nil || guildID == "" {
		return
	}
	g := t.guild(guildID)
	g.mu.Lock()
	defer g.mu.Unlock()
	t.refreshLocked(guildID, g)
}

func (t *InviteTracker) refreshLocked(guildID string, g *guildInvites) map[string]inviteState {
	list, err := t.api.GuildInvites(guildID)
	if err != nil {
		g.ready, g.lastErr = false, "Champion cannot read this server's invites. Give the bot the Manage Server permission."
		slog.Warn("component=invites", "msg", "invite snapshot failed", "guild", guildID, "err", err.Error())
		return nil
	}
	now := toState(list)
	g.invites, g.ready, g.lastErr, g.lastSync = now, true, "", t.now()
	return now
}

// InviteCreated records a new invite at its current use count so the next join can be matched.
func (t *InviteTracker) InviteCreated(e *discordgo.InviteCreate) {
	if t == nil || e == nil || e.Invite == nil || e.GuildID == "" {
		return
	}
	g := t.guild(e.GuildID)
	g.mu.Lock()
	defer g.mu.Unlock()
	for code, s := range toState([]*discordgo.Invite{e.Invite}) {
		g.invites[code] = s
	}
}

// MemberJoined attributes a join and stores it. Safe to call from a gateway handler goroutine.
func (t *InviteTracker) MemberJoined(ctx context.Context, guildID, memberID string, bot bool) {
	if t == nil || guildID == "" || memberID == "" || bot {
		return
	}
	g := t.guild(guildID)
	g.mu.Lock()
	before, wasReady := g.invites, g.ready
	after := t.refreshLocked(guildID, g)
	g.mu.Unlock()
	join := repository.InviteJoin{GuildDiscordID: guildID, MemberDiscordID: memberID, JoinedAt: t.now().UTC()}
	if wasReady && after != nil {
		if code, ok := AttributeInvite(before, after); ok {
			s := before[code]
			if a, found := after[code]; found {
				s = a
			}
			join.Code, join.InviterDiscordID, join.InviterName, join.Attributed = code, s.inviterID, s.inviterName, true
		}
	}
	if t.store != nil {
		if err := t.store.RecordInviteJoin(ctx, join); err != nil {
			slog.Warn("component=invites", "msg", "record join failed", "guild", guildID, "err", err.Error())
		}
	}
}

// MemberLeft stamps the member's latest join as left.
func (t *InviteTracker) MemberLeft(ctx context.Context, guildID, memberID string) {
	if t == nil || t.store == nil || guildID == "" || memberID == "" {
		return
	}
	if err := t.store.RecordInviteLeave(ctx, guildID, memberID, t.now().UTC()); err != nil {
		slog.Warn("component=invites", "msg", "record leave failed", "guild", guildID, "err", err.Error())
	}
}

// Status reports whether joins in a guild are being attributed, and why not.
func (t *InviteTracker) Status(guildID string) (ready bool, problem string, lastSync time.Time) {
	if t == nil {
		return false, "Invite tracking is not running.", time.Time{}
	}
	g := t.guild(guildID)
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.ready && g.lastErr == "" {
		return false, "Invite tracking starts once the bot has read this server's invites.", g.lastSync
	}
	return g.ready, g.lastErr, g.lastSync
}

// AttributeInvite finds the single invite a join used: the one whose use count went up, or (for
// a last-use invite Discord deleted on use) the one that vanished at max_uses-1. Anything else is
// ambiguous (vanity URL, several changes at once) and stays unattributed rather than guessed.
func AttributeInvite(before, after map[string]inviteState) (string, bool) {
	var grew []string
	for code, a := range after {
		if b, ok := before[code]; ok && a.uses > b.uses {
			grew = append(grew, code)
		} else if !ok && a.uses > 0 {
			grew = append(grew, code) // created and used between snapshots
		}
	}
	if len(grew) == 1 {
		return grew[0], true
	}
	if len(grew) > 1 {
		return "", false
	}
	var vanished []string
	for code, b := range before {
		if _, ok := after[code]; !ok && b.maxUses > 0 && b.uses+1 >= b.maxUses {
			vanished = append(vanished, code)
		}
	}
	if len(vanished) == 1 {
		return vanished[0], true
	}
	return "", false
}
