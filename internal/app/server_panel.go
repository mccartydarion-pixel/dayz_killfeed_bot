package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/dayzmap"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/livemap"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// serverPanelBackend serves the Discord /server panel (internal/discord/server_panel.go). Its
// actions use the same staff levels, Nitrado credential, rate limits and audit log as the
// website's Server Hub restart and stop.
type serverPanelBackend struct {
	a   *App
	now func() time.Time

	mu       sync.Mutex
	statuses map[int64]panelStatusEntry
	// Used only when the website API (which owns the shared limiters) is not running.
	ownRestartLimiter, ownStopLimiter, ownActionLimiter *saasRateLimiter
}

type panelStatusEntry struct {
	status discord.PanelStatus
	at     time.Time
}

// panelStatusTTL is how long a panel reuses a status read; Refresh reads again once the last read
// is panelRefreshMin old, so a burst of clicks costs Nitrado one read.
const (
	panelStatusTTL  = 20 * time.Second
	panelRefreshMin = 5 * time.Second
)

func newServerPanelBackend(a *App) *serverPanelBackend {
	return &serverPanelBackend{a: a, now: time.Now, statuses: map[int64]panelStatusEntry{},
		ownRestartLimiter: newSaaSRateLimiter(5*time.Minute, 1), ownStopLimiter: newSaaSRateLimiter(time.Minute, 1),
		ownActionLimiter: newSaaSRateLimiter(time.Minute, 20)}
}

// limiter is the rate limit of action: the website's own, keyed by the acting Discord user as
// there, so the panel and the website share each person's budget (restart: one per 5 minutes;
// start and stop: one a minute).
func (b *serverPanelBackend) limiter(action discord.PanelAction) *saasRateLimiter {
	if action == discord.PanelRestart {
		if b.a.saasServerRestartLimiter != nil {
			return b.a.saasServerRestartLimiter
		}
		return b.ownRestartLimiter
	}
	if b.a.saasServerStopLimiter != nil {
		return b.a.saasServerStopLimiter
	}
	return b.ownStopLimiter
}

func (b *serverPanelBackend) servers(ctx context.Context, discordGuildID string) ([]repository.PanelServer, error) {
	if b.a.ServerPanels == nil {
		return nil, errors.New("server panel repository unavailable")
	}
	return b.a.ServerPanels.Servers(ctx, discordGuildID)
}

// server is installationID's row, when it is an installation of discordGuildID.
func (b *serverPanelBackend) server(ctx context.Context, discordGuildID string, installationID int64) (repository.PanelServer, error) {
	rows, err := b.servers(ctx, discordGuildID)
	if err != nil {
		return repository.PanelServer{}, err
	}
	for _, row := range rows {
		if row.InstallationID == installationID {
			return row, nil
		}
	}
	return repository.PanelServer{}, discord.ErrPanelServerNotFound
}

func (b *serverPanelBackend) PanelServers(ctx context.Context, discordGuildID string) ([]discord.PanelServer, error) {
	rows, err := b.servers(ctx, discordGuildID)
	if err != nil {
		return nil, err
	}
	out := make([]discord.PanelServer, 0, len(rows))
	for _, row := range rows {
		out = append(out, discord.PanelServer{InstallationID: row.InstallationID, Name: row.Name, MapName: panelMapName(row.MapKey)})
	}
	return out, nil
}

func panelMapName(key string) string {
	if m, ok := dayzmap.Lookup(key); ok {
		return m.DisplayName
	}
	return ""
}

func (b *serverPanelBackend) PanelLevel(ctx context.Context, c discord.PanelCaller, installationID int64) (permissions.Level, error) {
	row, err := b.server(ctx, c.DiscordGuildID, installationID)
	if err != nil {
		return permissions.LevelNone, err
	}
	var mapped []string
	if b.a.Permissions != nil && len(c.RoleIDs) > 0 {
		if mapped, err = b.a.Permissions.LevelsForRoles(ctx, installationID, c.RoleIDs); err != nil {
			return permissions.LevelNone, err
		}
	}
	return panelLevel(row.OwnerDiscordID != "" && row.OwnerDiscordID == c.DiscordUserID, c.DiscordAdmin, mapped), nil
}

// panelLevel is a panel user's staff level: Owner for the organization's owner; else the highest
// level their Discord roles map to, and at least Administrator for a member with Discord's
// Administrator or Manage Server permission (as for every admin slash command).
func panelLevel(isOwner, discordAdmin bool, mappedLevels []string) permissions.Level {
	if isOwner {
		return permissions.LevelOwner
	}
	best := permissions.LevelNone
	for _, name := range mappedLevels {
		if l, ok := permissions.ParseLevel(name); ok && l > best {
			best = l
		}
	}
	if discordAdmin && best < permissions.LevelAdministrator {
		best = permissions.LevelAdministrator
	}
	return best
}

func (b *serverPanelBackend) client(ctx context.Context, row repository.PanelServer) (*nitrado.Client, error) {
	if b.a.SaaSCredentials == nil || b.a.CredentialCipher == nil {
		return nil, discord.ErrPanelNitrado
	}
	client, _, msg := b.a.nitradoClientForOrganization(ctx, row.OrganizationID)
	if client == nil {
		slog.Warn("component=discord", "msg", "server panel has no nitrado client", "installation_id", row.InstallationID, "reason", msg)
		return nil, discord.ErrPanelNitrado
	}
	return client, nil
}

func (b *serverPanelBackend) PanelStatus(ctx context.Context, discordGuildID string, installationID int64, fresh bool) (discord.PanelStatus, error) {
	row, err := b.server(ctx, discordGuildID, installationID)
	if err != nil {
		return discord.PanelStatus{}, err
	}
	now := b.now()
	b.mu.Lock()
	cached, ok := b.statuses[installationID]
	b.mu.Unlock()
	if ok {
		age := now.Sub(cached.at)
		if age < panelRefreshMin || (!fresh && age < panelStatusTTL) {
			return cached.status, nil
		}
	}
	client, err := b.client(ctx, row)
	if err != nil {
		return discord.PanelStatus{}, err
	}
	live, err := client.GameserverLive(ctx, row.ServiceID)
	if err != nil {
		return discord.PanelStatus{}, err
	}
	st := discord.PanelStatus{State: live.Status, Capacity: live.Capacity(), ReadAt: now}
	// Player counts are shown only when Nitrado's answer is provably this server's.
	if live.BelongsTo(row.ServiceID) {
		st.Players = live.PlayerCurrent
	}
	if tasks, err := client.ListScheduledTasks(ctx, row.ServiceID); err == nil {
		if at, ok := livemap.NextRestart(liveMapTasks(tasks), now); ok {
			st.NextRestart = &at
		}
	}
	b.mu.Lock()
	b.statuses[installationID] = panelStatusEntry{status: st, at: now}
	b.mu.Unlock()
	return st, nil
}

// panelAuditAction is the audit log action of each panel action (the website writes the same
// SERVER_RESTART and SERVER_STOP).
var panelAuditAction = map[discord.PanelAction]string{
	discord.PanelRestart: "SERVER_RESTART",
	discord.PanelStart:   "SERVER_START",
	discord.PanelStop:    "SERVER_STOP",
}

func (b *serverPanelBackend) PanelAct(ctx context.Context, c discord.PanelCaller, installationID int64, action discord.PanelAction, reason string) error {
	capability, ok := discord.PanelActionCapability(action)
	if !ok {
		return errors.New("unknown server panel action")
	}
	row, err := b.server(ctx, c.DiscordGuildID, installationID)
	if err != nil {
		return err
	}
	level, err := b.PanelLevel(ctx, c, installationID)
	if err != nil {
		return err
	}
	if !permissions.Allows(level, capability) {
		return discord.ErrPanelForbidden
	}
	if !b.limiter(action).Allow(c.DiscordUserID) {
		return discord.ErrPanelRateLimited
	}
	client, err := b.client(ctx, row)
	if err != nil {
		return err
	}
	switch action {
	case discord.PanelStop:
		err = client.Stop(ctx, row.ServiceID, reason)
	default:
		// Nitrado's restart also starts a stopped server.
		err = client.Restart(ctx, row.ServiceID, reason)
	}
	b.audit(ctx, c, row, panelAuditAction[action], "", reason, err == nil, nil)
	b.mu.Lock()
	delete(b.statuses, installationID)
	b.mu.Unlock()
	if err != nil {
		slog.Warn("component=discord", "msg", "server panel action refused by nitrado", "action", string(action), "installation_id", installationID, "err", err.Error())
		return discord.ErrPanelNitrado
	}
	return nil
}

// account is the Champion account of a Discord user, or nil when they have none.
func (b *serverPanelBackend) account(ctx context.Context, discordUserID string) *int64 {
	if b.a.SaaSUsers == nil {
		return nil
	}
	user, err := b.a.SaaSUsers.GetByDiscordID(ctx, discordUserID)
	if err != nil || user == nil {
		return nil
	}
	return &user.ID
}

// audit records a panel action in the admin audit log, best-effort, as the website does. extra
// joins the after-state, which always names the panel as the source.
func (b *serverPanelBackend) audit(ctx context.Context, c discord.PanelCaller, row repository.PanelServer, action, target, reason string, ok bool, extra map[string]any) {
	if b.a.AdminAudit == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	installationID := row.InstallationID
	entry := repository.AuditEntry{
		OrganizationID: row.OrganizationID,
		InstallationID: &installationID,
		ActorUserID:    b.account(writeCtx, c.DiscordUserID),
		ActorDiscordID: c.DiscordUserID,
		Action:         action,
		Target:         target,
		Reason:         reason,
		Result:         "success",
	}
	if !ok {
		entry.Result = "failure"
	}
	after := map[string]any{"source": "discord_panel"}
	for k, v := range extra {
		after[k] = v
	}
	if raw, err := json.Marshal(after); err == nil {
		entry.AfterState = raw
	}
	if err := b.a.AdminAudit.Record(writeCtx, entry); err != nil {
		slog.Warn("component=discord", "msg", "server panel audit write failed", "action", action, "err", err.Error())
	}
}

// listServer is installationID's row when the caller may edit list there.
func (b *serverPanelBackend) listServer(ctx context.Context, c discord.PanelCaller, installationID int64, list discord.PanelList) (repository.PanelServer, error) {
	capability, ok := discord.PanelListCapability(list)
	if !ok {
		return repository.PanelServer{}, errors.New("unknown server panel list")
	}
	row, err := b.server(ctx, c.DiscordGuildID, installationID)
	if err != nil {
		return repository.PanelServer{}, err
	}
	level, err := b.PanelLevel(ctx, c, installationID)
	if err != nil {
		return repository.PanelServer{}, err
	}
	if !permissions.Allows(level, capability) {
		return repository.PanelServer{}, discord.ErrPanelForbidden
	}
	return row, nil
}

// readPanelList reads one of the three lists from Nitrado.
func readPanelList(ctx context.Context, client *nitrado.Client, serviceID string, list discord.PanelList) ([]string, error) {
	var names []string
	var err error
	switch list {
	case discord.PanelWhitelist:
		names, err = client.Whitelist(ctx, serviceID)
	case discord.PanelBanlist:
		names, err = client.Banlist(ctx, serviceID)
	default:
		names, err = client.PriorityList(ctx, serviceID)
	}
	if errors.Is(err, nitrado.ErrAccessListUnsupported) || errors.Is(err, nitrado.ErrPriorityUnsupported) {
		return nil, discord.ErrPanelListUnsupported
	}
	if err != nil {
		slog.Warn("component=discord", "msg", "server panel list read failed", "list", string(list), "err", err.Error())
		return nil, discord.ErrPanelNitrado
	}
	return names, nil
}

func (b *serverPanelBackend) PanelListNames(ctx context.Context, c discord.PanelCaller, installationID int64, list discord.PanelList) ([]string, error) {
	row, err := b.listServer(ctx, c, installationID, list)
	if err != nil {
		return nil, err
	}
	client, err := b.client(ctx, row)
	if err != nil {
		return nil, err
	}
	return readPanelList(ctx, client, row.ServiceID, list)
}

// panelListAudit is the audit log action of each list change (the website writes the same).
var panelListAudit = map[discord.PanelList][2]string{
	discord.PanelWhitelist: {"WHITELIST_ADD", "WHITELIST_REMOVE"},
	discord.PanelBanlist:   {"BANLIST_ADD", "BANLIST_REMOVE"},
	discord.PanelPriority:  {"PRIORITY_ADD", "PRIORITY_REMOVE"},
}

func (b *serverPanelBackend) PanelListChange(ctx context.Context, c discord.PanelCaller, installationID int64, list discord.PanelList, name, reason string, add bool) error {
	name = strings.TrimSpace(name)
	if !nitrado.ValidPriorityName(name) {
		return discord.ErrPanelBadName
	}
	row, err := b.listServer(ctx, c, installationID, list)
	if err != nil {
		return err
	}
	// The website's admin-action limit, keyed by the acting Discord user as there.
	limiter := b.a.saasAdminActionLimiter
	if limiter == nil {
		limiter = b.ownActionLimiter
	}
	if !limiter.Allow(c.DiscordUserID) {
		return discord.ErrPanelRateLimited
	}
	client, err := b.client(ctx, row)
	if err != nil {
		return err
	}
	action := panelListAudit[list][1]
	if add {
		action = panelListAudit[list][0]
	}
	if list == discord.PanelPriority {
		return b.changePriority(ctx, c, row, client, name, reason, add, action)
	}
	// Names are matched without regard to case, as Nitrado and the game do. A change that would do
	// nothing does nothing, and a removal uses the name as the list holds it, so Champion's record
	// (kept per exact name) stays in step with Nitrado's list.
	current, err := readPanelList(ctx, client, row.ServiceID, list)
	if err != nil {
		return err
	}
	at := priorityIndex(current, name)
	if add == (at >= 0) {
		return nil
	}
	if !add {
		name = current[at]
	}
	switch {
	case list == discord.PanelWhitelist && add:
		err = client.WhitelistAdd(ctx, row.ServiceID, name)
	case list == discord.PanelWhitelist:
		err = client.WhitelistRemove(ctx, row.ServiceID, name)
	case add:
		err = client.BanlistAdd(ctx, row.ServiceID, name)
	default:
		err = client.BanlistRemove(ctx, row.ServiceID, name)
	}
	if errors.Is(err, nitrado.ErrAccessListUnsupported) {
		return discord.ErrPanelListUnsupported
	}
	if err != nil {
		b.audit(ctx, c, row, action, name, reason, false, nil)
		slog.Warn("component=discord", "msg", "server panel list change refused by nitrado", "list", string(list), "err", err.Error())
		return discord.ErrPanelNitrado
	}
	// Champion's own record of the entry, as the website keeps it. Nitrado's list is what counts,
	// so a record that can't be written (the name was already recorded, or was added outside
	// Champion) doesn't fail the change.
	if b.a.ClientAdmin != nil {
		listType := "WHITELIST"
		if list == discord.PanelBanlist {
			listType = "BANLIST"
		}
		actor := b.account(ctx, c.DiscordUserID)
		var recErr error
		if add {
			_, recErr = b.a.ClientAdmin.AddAccessEntry(ctx, installationID, listType, name, reason, "", nil, actor)
		} else if _, recErr = b.a.ClientAdmin.RemoveAccessEntry(ctx, installationID, listType, name, actor); errors.Is(recErr, repository.ErrAccessEntryNotFound) {
			recErr = nil
		}
		if recErr != nil {
			slog.Info("component=discord", "msg", "server panel access record not written", "list", listType, "err", recErr.Error())
		}
	}
	b.audit(ctx, c, row, action, name, reason, true, nil)
	return nil
}

// changePriority edits the priority queue the way the website does: read the list, change one
// name and write it back, under the lock the perk store and rewards pass also take.
func (b *serverPanelBackend) changePriority(ctx context.Context, c discord.PanelCaller, row repository.PanelServer, client *nitrado.Client, name, reason string, add bool, action string) error {
	defer nitrado.LockSetting(row.ServiceID, nitrado.SettingPriority)()
	current, err := readPanelList(ctx, client, row.ServiceID, discord.PanelPriority)
	if err != nil {
		return err
	}
	at := priorityIndex(current, name)
	var updated []string
	switch {
	case add && at >= 0, !add && at < 0:
		return nil
	case add && len(current) >= nitrado.MaxPriorityEntries:
		return discord.ErrPanelListFull
	case add:
		updated = append(append(make([]string, 0, len(current)+1), current...), name)
	default:
		updated = append(append(make([]string, 0, len(current)), current[:at]...), current[at+1:]...)
	}
	if err := client.SetPriorityList(ctx, row.ServiceID, updated); err != nil {
		b.audit(ctx, c, row, action, name, reason, false, nil)
		slog.Warn("component=discord", "msg", "server panel priority write refused by nitrado", "err", err.Error())
		return discord.ErrPanelNitrado
	}
	b.audit(ctx, c, row, action, name, reason, true, map[string]any{"count": len(updated)})
	return nil
}
