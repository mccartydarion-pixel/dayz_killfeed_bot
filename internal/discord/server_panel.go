package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// /server panel: a private control panel for the Nitrado game server. It shows the server's live
// status and has Restart / Start / Stop / Refresh buttons. Who may press what is the Champion
// staff level the website's Staff page maps the member's Discord roles to (a Discord member with
// Administrator or Manage Server counts as an Administrator, as for every admin command). The
// level is read again on every click, so removing a role takes effect at once, and every action
// goes through the same rate limits and audit log as the website's Server Hub.

// Custom ids. Buttons that change the panel start with ServerPanelPrefix; the two that open a
// confirmation form start with ServerPanelAskPrefix (a form cannot be deferred); the forms
// themselves are ServerPanelPrefix + "do:".
const (
	ServerPanelPrefix    = "srvpanel:"
	ServerPanelAskPrefix = "srvpanel:ask:"
	serverPanelDoPrefix  = "srvpanel:do:"
	serverPanelPickID    = "srvpanel:pick"

	serverPanelReasonInput  = "reason"
	serverPanelConfirmInput = "confirm"
)

// PanelAction is a power action the panel sends to Nitrado.
type PanelAction string

const (
	PanelRestart PanelAction = "restart"
	PanelStart   PanelAction = "start"
	PanelStop    PanelAction = "stop"
)

// panelActionCapability is the staff capability each action needs: starting a stopped server is
// as safe as a restart, stopping it needs an Administrator.
var panelActionCapability = map[PanelAction]permissions.Capability{
	PanelRestart: permissions.CapServerRestart,
	PanelStart:   permissions.CapServerRestart,
	PanelStop:    permissions.CapServerStop,
}

// PanelActionCapability is the staff capability action needs.
func PanelActionCapability(action PanelAction) (permissions.Capability, bool) {
	c, ok := panelActionCapability[action]
	return c, ok
}

// PanelCaller is the member pressing a panel button.
type PanelCaller struct {
	DiscordGuildID, DiscordUserID string
	RoleIDs                       []string
	// DiscordAdmin is Discord's Administrator or Manage Server permission.
	DiscordAdmin bool
}

// PanelServer is a game server the panel can control.
type PanelServer struct {
	InstallationID int64
	Name           string
	MapName        string
}

// PanelStatus is what Nitrado says about the server.
type PanelStatus struct {
	// State is Nitrado's gameserver status ("started", "stopped", "restarting", ...); "" when
	// Nitrado could not be read.
	State       string
	Players     *int
	Capacity    int
	NextRestart *time.Time
	ReadAt      time.Time
}

// ServerPanelBackend is what the panel needs from the app. Every method is scoped to the Discord
// server the interaction came from: an installation of another Discord server is
// ErrPanelServerNotFound.
type ServerPanelBackend interface {
	PanelServers(ctx context.Context, discordGuildID string) ([]PanelServer, error)
	PanelLevel(ctx context.Context, c PanelCaller, installationID int64) (permissions.Level, error)
	// PanelStatus reads the server's status; fresh skips the short cache.
	PanelStatus(ctx context.Context, discordGuildID string, installationID int64, fresh bool) (PanelStatus, error)
	// PanelAct checks the caller's level and rate limit, sends the action to Nitrado and records
	// it in the audit log.
	PanelAct(ctx context.Context, c PanelCaller, installationID int64, action PanelAction, reason string) error
}

var (
	ErrPanelServerNotFound = errors.New("server panel: server not found")
	ErrPanelForbidden      = errors.New("server panel: staff level too low")
	ErrPanelRateLimited    = errors.New("server panel: rate limited")
	ErrPanelNitrado        = errors.New("server panel: nitrado refused")
)

// panelPowerLock is how long the power buttons stay locked after a restart, start or stop, so two
// staff members can't send it twice while the server comes back.
const panelPowerLock = 2 * time.Minute

type ServerPanelHandler struct {
	backend ServerPanelBackend
	now     func() time.Time

	mu     sync.Mutex
	locked map[int64]panelLock
}

type panelLock struct {
	action PanelAction
	at     time.Time
}

func NewServerPanelHandler(b ServerPanelBackend) *ServerPanelHandler {
	return &ServerPanelHandler{backend: b, now: time.Now, locked: map[int64]panelLock{}}
}

func panelCallerOf(i *discordgo.InteractionCreate) (PanelCaller, bool) {
	if i == nil || i.Member == nil || i.Member.User == nil || i.GuildID == "" {
		return PanelCaller{}, false
	}
	return PanelCaller{
		DiscordGuildID: i.GuildID,
		DiscordUserID:  i.Member.User.ID,
		RoleIDs:        i.Member.Roles,
		DiscordAdmin:   i.Member.Permissions&(discordgo.PermissionAdministrator|discordgo.PermissionManageServer) != 0,
	}, true
}

// lockOf returns the power lock on installationID, if one is still running.
func (h *ServerPanelHandler) lockOf(installationID int64) (panelLock, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.locked[installationID]
	if !ok {
		return panelLock{}, false
	}
	if h.now().Sub(l.at) >= panelPowerLock {
		delete(h.locked, installationID)
		return panelLock{}, false
	}
	return l, true
}

// takeLock locks installationID's power buttons unless they are locked already.
func (h *ServerPanelHandler) takeLock(installationID int64, action PanelAction) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l, ok := h.locked[installationID]; ok && h.now().Sub(l.at) < panelPowerLock {
		return false
	}
	h.locked[installationID] = panelLock{action: action, at: h.now()}
	return true
}

func (h *ServerPanelHandler) releaseLock(installationID int64) {
	h.mu.Lock()
	delete(h.locked, installationID)
	h.mu.Unlock()
}

const (
	panelNoServersText = "This Discord server has no game server set up on the Champion website yet. Connect one in the Server Hub, then try again."
	panelNoStaffText   = "You need a staff role to use the server panel. The server owner can give your Discord role a staff level on the Champion website (Server Hub › Staff)."
	panelGoneText      = "That server is no longer set up on this Discord server. Run `/server panel` again."
	panelLockedText    = "A restart, start or stop was just sent for this server. Wait until the power buttons unlock."
)

// HandleCommand answers /server panel with the panel for the Discord server's first game server.
func (h *ServerPanelHandler) HandleCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	caller, ok := panelCallerOf(i)
	if !ok || h == nil || h.backend == nil {
		respondEphemeral(s, i, ReplyIsUnavailable("The server panel"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	servers, err := h.backend.PanelServers(ctx, caller.DiscordGuildID)
	if err != nil {
		slog.Warn("component=discord", "msg", "server panel servers lookup failed", "err", err.Error())
		respondEphemeral(s, i, ReplyCouldNot("load this Discord server's game servers"))
		return
	}
	if len(servers) == 0 {
		respondEphemeral(s, i, panelNoServersText)
		return
	}
	h.show(ctx, s, i, caller, servers, servers[0].InstallationID, false, respondPrivate, nil)
}

// show renders the panel for installationID and answers with send (a new private message, or an
// update of the panel the button sits on). override replaces the status Nitrado reports (right
// after an action, Nitrado still reports the old state).
func (h *ServerPanelHandler) show(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller PanelCaller, servers []PanelServer, installationID int64, fresh bool,
	send func(*discordgo.Session, *discordgo.InteractionCreate, *discordgo.InteractionResponseData), override func(*PanelStatus)) {
	var current *PanelServer
	for idx := range servers {
		if servers[idx].InstallationID == installationID {
			current = &servers[idx]
		}
	}
	if current == nil {
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: panelGoneText})
		return
	}
	level, err := h.backend.PanelLevel(ctx, caller, installationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	if level < permissions.LevelGatekeeper {
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: panelNoStaffText})
		return
	}
	status, err := h.backend.PanelStatus(ctx, caller.DiscordGuildID, installationID, fresh)
	if err != nil {
		slog.Warn("component=discord", "msg", "server panel status failed", "err", err.Error())
		status = PanelStatus{ReadAt: h.now()}
	}
	if override != nil {
		override(&status)
	}
	var lock *panelLock
	if l, ok := h.lockOf(installationID); ok {
		lock = &l
	}
	send(s, i, BuildServerPanel(servers, *current, status, level, lock, h.now()))
}

// HandleComponent handles every panel button, the server picker and the two confirmation forms.
func (h *ServerPanelHandler) HandleComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	caller, ok := panelCallerOf(i)
	if !ok || h == nil || h.backend == nil {
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: ReplyIsUnavailable("The server panel")})
		return
	}
	var customID string
	switch i.Type {
	case discordgo.InteractionMessageComponent:
		customID = i.MessageComponentData().CustomID
	case discordgo.InteractionModalSubmit:
		customID = i.ModalSubmitData().CustomID
	default:
		return
	}
	if strings.HasPrefix(customID, ServerPanelAskPrefix) {
		h.handleAsk(s, i, caller, strings.TrimPrefix(customID, ServerPanelAskPrefix))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if customID == serverPanelPickID {
		values := i.MessageComponentData().Values
		if len(values) != 1 {
			return
		}
		installationID, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil {
			return
		}
		h.refresh(ctx, s, i, caller, installationID, false, nil)
		return
	}
	if strings.HasPrefix(customID, serverPanelDoPrefix) {
		action, installationID, ok := parsePanelTarget(strings.TrimPrefix(customID, serverPanelDoPrefix))
		if !ok || action == PanelStart {
			return
		}
		data := i.ModalSubmitData()
		reason := strings.TrimSpace(modalValue(data, serverPanelReasonInput))
		if action == PanelStop && strings.TrimSpace(modalValue(data, serverPanelConfirmInput)) != "STOP" {
			respondPrivate(s, i, &discordgo.InteractionResponseData{Content: "The server was **not** stopped: type `STOP` in capital letters to confirm."})
			return
		}
		h.act(ctx, s, i, caller, installationID, action, reason)
		return
	}
	rest := strings.TrimPrefix(customID, ServerPanelPrefix)
	if id, ok := strings.CutPrefix(rest, "refresh:"); ok {
		installationID, err := strconv.ParseInt(id, 10, 64)
		if err == nil {
			h.refresh(ctx, s, i, caller, installationID, true, nil)
		}
		return
	}
	if action, installationID, ok := parsePanelTarget(rest); ok && action == PanelStart {
		h.act(ctx, s, i, caller, installationID, PanelStart, "")
	}
}

func (h *ServerPanelHandler) refresh(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller PanelCaller, installationID int64, fresh bool, override func(*PanelStatus)) {
	servers, err := h.backend.PanelServers(ctx, caller.DiscordGuildID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	h.show(ctx, s, i, caller, servers, installationID, fresh, respondUpdate, override)
}

// handleAsk opens the confirmation form of a restart or stop. A form must be the first answer, so
// only quick checks run first.
func (h *ServerPanelHandler) handleAsk(s *discordgo.Session, i *discordgo.InteractionCreate, caller PanelCaller, target string) {
	action, installationID, ok := parsePanelTarget(target)
	if !ok || action == PanelStart {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	level, err := h.backend.PanelLevel(ctx, caller, installationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	if !permissions.Allows(level, panelActionCapability[action]) {
		h.reportError(s, i, ErrPanelForbidden)
		return
	}
	if _, locked := h.lockOf(installationID); locked {
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: panelLockedText})
		return
	}
	_ = respondModalData(s, i, BuildServerPanelForm(action, installationID))
}

// act sends a power action and shows the panel with the server's new state.
func (h *ServerPanelHandler) act(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller PanelCaller, installationID int64, action PanelAction, reason string) {
	// The panel is updated and a private note follows, so acknowledge as an update first: an
	// immediate update would leave no room for the note.
	deferAs(s, i, AckUpdate)
	if action != PanelStart && reason == "" {
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: "A reason is needed. It is saved in the audit log."})
		return
	}
	if !h.takeLock(installationID, action) {
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: panelLockedText})
		return
	}
	if err := h.backend.PanelAct(ctx, caller, installationID, action, reason); err != nil {
		h.releaseLock(installationID)
		h.reportError(s, i, err)
		return
	}
	h.refresh(ctx, s, i, caller, installationID, false, func(st *PanelStatus) {
		st.State = map[PanelAction]string{PanelRestart: "restarting", PanelStart: "starting", PanelStop: "stopping"}[action]
		st.Players = nil
	})
	respondPrivate(s, i, &discordgo.InteractionResponseData{Content: panelActionSentText(action)})
}

func panelActionSentText(action PanelAction) string {
	switch action {
	case PanelRestart:
		return "🔄 Restart sent to Nitrado. Players are disconnected now and the server is usually back in 2–5 minutes. Press **Refresh** to check."
	case PanelStart:
		return "▶️ Start sent to Nitrado. The server is usually up in 2–5 minutes. Press **Refresh** to check."
	default:
		return "⏹️ Stop sent to Nitrado. The server stays off until someone presses **Start**."
	}
}

func (h *ServerPanelHandler) reportError(s *discordgo.Session, i *discordgo.InteractionCreate, err error) {
	text := ReplyCouldNot("reach the game server")
	switch {
	case errors.Is(err, ErrPanelServerNotFound):
		text = panelGoneText
	case errors.Is(err, ErrPanelForbidden):
		text = "Your staff level doesn't allow that. Restart and Start need Moderator, Stop needs Administrator. The server owner sets staff levels on the Champion website (Server Hub › Staff)."
	case errors.Is(err, ErrPanelRateLimited):
		text = "Slow down: each person can restart once every 5 minutes and start or stop once a minute."
	case errors.Is(err, ErrPanelNitrado):
		text = "Nitrado didn't accept the request. Check the server in the Nitrado web interface, then try again."
	default:
		slog.Warn("component=discord", "msg", "server panel action failed", "err", err.Error())
	}
	respondPrivate(s, i, &discordgo.InteractionResponseData{Content: text})
}

// parsePanelTarget reads "<action>:<installation id>".
func parsePanelTarget(s string) (PanelAction, int64, bool) {
	name, id, ok := strings.Cut(s, ":")
	if !ok {
		return "", 0, false
	}
	action := PanelAction(name)
	if _, known := panelActionCapability[action]; !known {
		return "", 0, false
	}
	installationID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || installationID <= 0 {
		return "", 0, false
	}
	return action, installationID, true
}

func panelTargetID(prefix string, action PanelAction, installationID int64) string {
	return prefix + string(action) + ":" + strconv.FormatInt(installationID, 10)
}

// panelStateView is how a Nitrado status is shown: its label, embed colour, and whether the
// server is down (Start instead of Restart) or between states (power buttons wait). An empty
// label is a status Nitrado did not give.
type panelStateView struct {
	label   string
	color   int
	stopped bool
	busy    bool
}

func panelState(state string) panelStateView {
	switch state {
	case "started":
		return panelStateView{label: "🟢 Online", color: presentation.Green}
	case "stopped", "suspended":
		return panelStateView{label: "🔴 Stopped", color: presentation.Red, stopped: true}
	case "restarting":
		return panelStateView{label: "🟡 Restarting", color: presentation.Amber, busy: true}
	case "starting":
		return panelStateView{label: "🟡 Starting", color: presentation.Amber, busy: true}
	case "stopping":
		return panelStateView{label: "🟡 Stopping", color: presentation.Amber, busy: true}
	case "":
		return panelStateView{color: presentation.Neutral}
	default:
		return panelStateView{label: "🟡 " + strings.ToUpper(state[:1]) + strings.ReplaceAll(state[1:], "_", " "), color: presentation.Amber, busy: true}
	}
}

// levelLabel names a staff level the way the website does.
func levelLabel(l permissions.Level) string {
	switch l {
	case permissions.LevelOwner:
		return "Owner"
	case permissions.LevelAdministrator:
		return "Administrator"
	case permissions.LevelModerator:
		return "Moderator"
	case permissions.LevelGatekeeper:
		return "Gatekeeper"
	}
	return "None"
}

// BuildServerPanel renders the panel: the server's status and the buttons level may use (the
// others are shown greyed out). A figure Nitrado did not give is left out.
func BuildServerPanel(servers []PanelServer, current PanelServer, st PanelStatus, level permissions.Level, lock *panelLock, now time.Time) *discordgo.InteractionResponseData {
	view := panelState(st.State)
	embed := &discordgo.MessageEmbed{
		Author: presentation.BrandAuthor(""),
		Title:  "🎛️ Server control",
		Color:  view.color,
		Footer: presentation.Footer(current.Name, "Staff only"),
	}
	var lines []string
	if view.label == "" {
		lines = append(lines, "Nitrado didn't answer, so the server's status is unknown. Press **Refresh** to try again.")
	} else {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Status", Value: view.label, Inline: true})
	}
	if st.Players != nil {
		players := strconv.Itoa(*st.Players)
		if st.Capacity > 0 {
			players += " / " + strconv.Itoa(st.Capacity)
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Players", Value: players, Inline: true})
	}
	if current.MapName != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Map", Value: current.MapName, Inline: true})
	}
	if st.NextRestart != nil {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Next scheduled restart", Value: presentation.TimestampWithRelative(*st.NextRestart)})
	}
	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Your staff level", Value: levelLabel(level) + " · greyed-out buttons need a higher level"})
	locked := lock != nil
	if locked {
		lines = append(lines, fmt.Sprintf("⏳ A %s was sent %s. The power buttons unlock %s.", lock.action, presentation.Timestamp(lock.at, 'R'), presentation.Timestamp(lock.at.Add(panelPowerLock), 'R')))
	}
	embed.Description = strings.Join(lines, "\n")
	presentation.StampEmbed(embed, st.ReadAt)

	allowed := func(c permissions.Capability) bool { return permissions.Allows(level, c) }
	id := current.InstallationID
	var power discordgo.Button
	if view.stopped {
		power = discordgo.Button{Label: "Start", Emoji: &discordgo.ComponentEmoji{Name: "▶️"}, Style: discordgo.SuccessButton,
			CustomID: panelTargetID(ServerPanelPrefix, PanelStart, id), Disabled: locked || !allowed(permissions.CapServerRestart)}
	} else {
		power = discordgo.Button{Label: "Restart", Emoji: &discordgo.ComponentEmoji{Name: "🔄"}, Style: discordgo.PrimaryButton,
			CustomID: panelTargetID(ServerPanelAskPrefix, PanelRestart, id), Disabled: locked || view.busy || !allowed(permissions.CapServerRestart)}
	}
	stop := discordgo.Button{Label: "Stop", Emoji: &discordgo.ComponentEmoji{Name: "⏹️"}, Style: discordgo.DangerButton,
		CustomID: panelTargetID(ServerPanelAskPrefix, PanelStop, id), Disabled: locked || view.busy || view.stopped || !allowed(permissions.CapServerStop)}
	// "↻" is not an emoji Discord accepts, so it is part of the label.
	refresh := discordgo.Button{Label: "↻ Refresh", Style: discordgo.SecondaryButton, CustomID: ServerPanelPrefix + "refresh:" + strconv.FormatInt(id, 10)}
	components := []discordgo.MessageComponent{}
	if len(servers) > 1 {
		options := make([]discordgo.SelectMenuOption, 0, len(servers))
		for idx, srv := range servers {
			if idx == 25 {
				break
			}
			options = append(options, discordgo.SelectMenuOption{Label: panelClip(srv.Name, 100), Value: strconv.FormatInt(srv.InstallationID, 10),
				Description: panelClip(srv.MapName, 100), Default: srv.InstallationID == id})
		}
		components = append(components, discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{CustomID: serverPanelPickID, Placeholder: "Pick a game server", Options: options},
		}})
	}
	components = append(components, discordgo.ActionsRow{Components: []discordgo.MessageComponent{power, stop, refresh}})
	return &discordgo.InteractionResponseData{
		Embeds:          []*discordgo.MessageEmbed{embed},
		Components:      components,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}
}

// BuildServerPanelForm is the confirmation form of a restart or stop.
func BuildServerPanelForm(action PanelAction, installationID int64) *discordgo.InteractionResponseData {
	reason := discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{
		CustomID: serverPanelReasonInput, Label: "Reason (saved in the audit log)", Style: discordgo.TextInputShort,
		Placeholder: "e.g. Server lag after the airdrop event", Required: true, MinLength: 3, MaxLength: 200,
	}}}
	if action == PanelStop {
		return &discordgo.InteractionResponseData{
			CustomID: panelTargetID(serverPanelDoPrefix, PanelStop, installationID),
			Title:    "Stop the server",
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{
					CustomID: serverPanelConfirmInput, Label: "Type STOP to confirm", Style: discordgo.TextInputShort,
					Placeholder: "STOP", Required: true, MinLength: 4, MaxLength: 4,
				}}},
				reason,
			},
		}
	}
	return &discordgo.InteractionResponseData{
		CustomID:   panelTargetID(serverPanelDoPrefix, PanelRestart, installationID),
		Title:      "Restart the server",
		Components: []discordgo.MessageComponent{reason},
	}
}

// panelClip shortens s to at most n runes (Discord's limit on select option text).
func panelClip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
