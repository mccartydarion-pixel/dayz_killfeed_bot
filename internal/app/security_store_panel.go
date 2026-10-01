package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Security Store panel (docs/SECURITY_STORE_PANEL.md): one message in a Discord
// channel the server owner picks, listing the base services on sale with their
// prices and a link to the store. The bot edits the same message when prices
// change and every 10 minutes; if it was deleted, it posts a new one.

const securityPanelRefreshEvery = 10 * time.Minute

type securityPanelSender interface {
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageEditComplex(m *discordgo.MessageEdit, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// securityPanelItems lists the services on sale and switched on, in store order.
func (a *App) securityPanelItems(ctx context.Context, s repository.SecurityScope) ([]discord.SecurityPanelItem, error) {
	sales := repository.NewSecurityServiceRepository(a.DB.Pool)
	var out []discord.SecurityPanelItem
	for _, id := range []string{repository.ServiceBaseRaidAlarm, repository.ServicePerimeterWatch, repository.ServiceBaseBlackBox,
		repository.ServiceFactionSecurity, repository.ServiceSentinelPro} {
		offer, err := sales.GetOffer(ctx, s, id)
		if err != nil {
			return nil, err
		}
		if !offer.Enabled {
			continue
		}
		on, err := a.securityServiceOn(ctx, s, id)
		if err != nil {
			return nil, err
		}
		if !on {
			continue
		}
		item := discord.SecurityPanelItem{ServiceID: id, PricePoints: offer.PricePoints, DurationDays: offer.DurationDays}
		if id == repository.ServiceSentinelPro {
			if item.Includes, err = a.sentinelProIncludes(ctx, s); err != nil {
				return nil, err
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// publishSecurityPanel posts or edits one panel and records the result.
func (a *App) publishSecurityPanel(ctx context.Context, sender securityPanelSender, p repository.SecurityStorePanel) error {
	s := repository.SecurityScope{InstallationID: p.InstallationID, GuildID: p.GuildID, ServerID: p.ServerID}
	repo := repository.NewSecurityStorePanelRepository(a.DB.Pool)
	items, err := a.securityPanelItems(ctx, s)
	if err != nil {
		return err
	}
	msg := discord.SecurityStorePanel(items, a.serverNameFunc()(p.ServerID), a.securityStoreURL(), time.Now())
	messageID := ""
	if p.MessageID != "" {
		edit := &discordgo.MessageEdit{ID: p.MessageID, Channel: p.ChannelID, Embeds: &msg.Embeds, AllowedMentions: msg.AllowedMentions}
		components := msg.Components
		if components == nil {
			components = []discordgo.MessageComponent{}
		}
		edit.Components = &components
		if _, err := sender.ChannelMessageEditComplex(edit); err == nil {
			return repo.MarkPosted(ctx, s, p.ChannelID, p.MessageID, "")
		} else if !discordNotFound(err) {
			_ = repo.MarkPosted(ctx, s, p.ChannelID, "", "Couldn't update the panel: "+err.Error())
			return err
		}
		// The message was deleted: post a new one below.
	}
	posted, err := sender.ChannelMessageSendComplex(p.ChannelID, msg)
	if err != nil {
		_ = repo.MarkPosted(ctx, s, p.ChannelID, "", "Couldn't post the panel (check the bot can send messages there): "+err.Error())
		return err
	}
	if posted != nil {
		messageID = posted.ID
	}
	return repo.MarkPosted(ctx, s, p.ChannelID, messageID, "")
}

func discordNotFound(err error) bool {
	var rest *discordgo.RESTError
	return errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode == http.StatusNotFound
}

// refreshSecurityPanel updates one server's panel in the background, if it has one.
func (a *App) refreshSecurityPanel(s repository.SecurityScope) {
	if a.DB == nil || a.DB.Pool == nil || a.Discord == nil || a.Discord.Session() == nil {
		return
	}
	session := a.Discord.Session()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("component=security_panel", "msg", "refresh panic recovered", "panic", fmt.Sprint(r))
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		p, err := repository.NewSecurityStorePanelRepository(a.DB.Pool).Get(ctx, s)
		if err != nil || p == nil || !p.Enabled {
			return
		}
		if err := a.publishSecurityPanel(ctx, session, *p); err != nil {
			slog.Warn("component=security_panel", "event", "refresh_failed", "installation_id", s.InstallationID, "err", err.Error())
		}
	}()
}

// startSecurityPanelWorker refreshes every switched-on panel every 10 minutes.
func (a *App) startSecurityPanelWorker(ctx context.Context) {
	if a.DB == nil || a.DB.Pool == nil || a.Discord == nil || a.Discord.Session() == nil {
		return
	}
	session := a.Discord.Session()
	go func() {
		t := time.NewTicker(securityPanelRefreshEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.refreshAllSecurityPanels(ctx, session)
			}
		}
	}()
}

func (a *App) refreshAllSecurityPanels(ctx context.Context, sender securityPanelSender) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=security_panel", "msg", "worker panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	panels, err := repository.NewSecurityStorePanelRepository(a.DB.Pool).Enabled(ctx)
	if err != nil {
		slog.Warn("component=security_panel", "event", "list_failed", "err", err.Error())
		return
	}
	for _, p := range panels {
		pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		if err := a.publishSecurityPanel(pctx, sender, p); err != nil {
			slog.Warn("component=security_panel", "event", "refresh_failed", "installation_id", p.InstallationID, "err", err.Error())
		}
		cancel()
	}
}

type securityPanelRequest struct {
	ChannelID string `json:"channelId"`
	Enabled   bool   `json:"enabled"`
}

// handleGetSecurityPanel is GET .../admin/case/security-panel (owner only).
func (a *App) handleGetSecurityPanel(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	s := repository.SecurityScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID}
	p, err := repository.NewSecurityStorePanelRepository(a.DB.Pool).Get(ctx, s)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the Security Store panel")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"panel": p})
}

// handleSetSecurityPanel is PUT .../admin/case/security-panel {channelId, enabled}.
// The channel must be a text channel in this installation's Discord server.
func (a *App) handleSetSecurityPanel(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	var req securityPanelRequest
	if err := readCaseBaseJSON(w, r, &req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid Security Store panel settings")
		return
	}
	req.ChannelID = strings.TrimSpace(req.ChannelID)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if a.saasDiscordVerifier == nil || a.SaaSInstallations == nil {
		writeSaaSError(w, codeDiscordUnavailable, "Discord bot session is unavailable")
		return
	}
	_, discordGuildID, errCode, errMsg := a.loadInstallationGuildSnowflake(ctx, ac.scope.OrganizationID, ac.scope.InstallationID)
	if errCode != "" {
		writeSaaSError(w, errCode, errMsg)
		return
	}
	channels, err := a.saasDiscordVerifier.ListGuildChannels(discordGuildID)
	if err != nil {
		writeSaaSError(w, codeDiscordUnavailable, "could not check your Discord channels")
		return
	}
	found := false
	for _, c := range channels {
		found = found || c.ID == req.ChannelID
	}
	if !found {
		writeSaaSError(w, codeInvalidRequest, "pick a text channel in this Discord server")
		return
	}
	var actor *int64
	if ac.user != nil {
		actor = &ac.user.ID
	}
	s := repository.SecurityScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID}
	repo := repository.NewSecurityStorePanelRepository(a.DB.Pool)
	p, err := repo.Set(ctx, s, req.ChannelID, req.Enabled, actor)
	if err != nil {
		slog.Warn("component=security_panel", "event", "set_failed", "err", err.Error())
		writeSaaSError(w, codeInvalidRequest, "could not save the Security Store panel")
		return
	}
	a.recordAudit(ctx, ac, "SECURITY_PANEL_SAVED", "security-panel", "", "success", nil,
		map[string]any{"channelId": p.ChannelID, "enabled": p.Enabled})
	// Post right away so the owner sees any permission problem now.
	if p.Enabled && a.Discord != nil && a.Discord.Session() != nil {
		if err := a.publishSecurityPanel(ctx, a.Discord.Session(), p); err != nil {
			slog.Warn("component=security_panel", "event", "first_post_failed", "installation_id", s.InstallationID, "err", err.Error())
		}
		if fresh, err := repo.Get(ctx, s); err == nil && fresh != nil {
			p = *fresh
		}
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"panel": p})
}
