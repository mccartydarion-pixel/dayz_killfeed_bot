package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/security"
)

const (
	serverConnectModalID = "champion_server_connect_modal"
	serverConnectTokenID = "champion_server_connect_token"
)

// ServerRuntime lets the /server command handler manage worker lifecycle
// without the discord package depending on the app package. Implemented by
// *app.App in production.
type ServerRuntime interface {
	// ConnectServer starts (or restarts) the ADM worker for a server and marks
	// this as a first-connect, so the worker begins at the log tail instead of
	// replaying pre-existing history.
	ConnectServer(ctx context.Context, serverID int64) error
	// DisconnectServer stops the ADM worker for a server, if running.
	DisconnectServer(serverID int64)
	// RepairServer restarts the ADM worker for an already-connected server
	// without resetting its checkpoint to the tail.
	RepairServer(ctx context.Context, serverID int64) error
}

type ServerCommandHandler struct {
	servers *repository.ServerRepository
	guilds  GuildStore
	cipher  *security.AESGCM
	runtime ServerRuntime
}

func NewServerCommandHandler(s *repository.ServerRepository, g GuildStore, cipher *security.AESGCM, runtime ServerRuntime) *ServerCommandHandler {
	return &ServerCommandHandler{servers: s, guilds: g, cipher: cipher, runtime: runtime}
}

func RegisterServerCommands(s *discordgo.Session, guildID string) error {
	applicationID, err := ApplicationID(s)
	if err != nil {
		return err
	}
	serviceIDOption := []*discordgo.ApplicationCommandOption{
		{Name: "service_id", Description: "DayZ service ID", Type: discordgo.ApplicationCommandOptionString, Required: true, Autocomplete: true},
	}
	cmd := &discordgo.ApplicationCommand{
		Name:        "server",
		Description: "Connect and manage DayZ game servers",
		Options: []*discordgo.ApplicationCommandOption{
			{Name: "connect", Description: "Securely connect a Nitrado API token", Type: discordgo.ApplicationCommandOptionSubCommand},
			{Name: "services", Description: "List DayZ services on the connected Nitrado account", Type: discordgo.ApplicationCommandOptionSubCommand},
			{Name: "select", Description: "Connect a DayZ service and start its killfeed", Type: discordgo.ApplicationCommandOptionSubCommand, Options: serviceIDOption},
			{Name: "disconnect", Description: "Disconnect a server (keeps history)", Type: discordgo.ApplicationCommandOptionSubCommand, Options: serviceIDOption},
			{Name: "repair", Description: "Re-validate and reattach a server's killfeed worker", Type: discordgo.ApplicationCommandOptionSubCommand, Options: serviceIDOption},
			{Name: "status", Description: "Show connected server status", Type: discordgo.ApplicationCommandOptionSubCommand},
		},
	}
	_, err = s.ApplicationCommandCreate(applicationID, guildID, cmd)
	return err
}

// Handle routes every interaction type /server can receive: the subcommands
// themselves, the connect modal submission, and service_id autocomplete.
func (h *ServerCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || h.servers == nil || h.guilds == nil || i == nil {
		respondEphemeral(s, i, "Server management is unavailable.")
		return
	}

	switch i.Type {
	case discordgo.InteractionApplicationCommandAutocomplete:
		h.handleAutocomplete(s, i)
		return
	case discordgo.InteractionModalSubmit:
		h.handleModalSubmit(s, i)
		return
	case discordgo.InteractionApplicationCommand:
		// falls through below
	default:
		return
	}

	if !isAdmin(s, i) {
		respondEphemeral(s, i, "⛔ You need Administrator or Manage Server permission to manage servers.")
		return
	}

	opts := i.ApplicationCommandData().Options
	if len(opts) == 0 {
		respondEphemeral(s, i, "Choose a /server subcommand.")
		return
	}
	sub := opts[0]

	switch sub.Name {
	case "connect":
		h.handleConnect(s, i)
	case "services":
		h.handleServices(s, i)
	case "select":
		h.handleSelect(s, i, sub)
	case "disconnect":
		h.handleDisconnect(s, i, sub)
	case "repair":
		h.handleRepair(s, i, sub)
	case "status":
		h.handleStatus(s, i)
	default:
		respondEphemeral(s, i, "Unknown /server subcommand.")
	}
}

// handleConnect opens a modal so the Nitrado API token is never typed into a
// visible channel message or logged anywhere.
func (h *ServerCommandHandler) handleConnect(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h.cipher == nil {
		respondEphemeral(s, i, "❌ Server connection is unavailable: credential encryption is not configured on this deployment.")
		return
	}
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: serverConnectModalID,
			Title:    "Connect Nitrado Server",
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					discordgo.TextInput{
						CustomID:    serverConnectTokenID,
						Label:       "Nitrado API Token",
						Style:       discordgo.TextInputShort,
						Placeholder: "Paste your Nitrado API token",
						Required:    true,
						MinLength:   10,
						MaxLength:   512,
					},
				}},
			},
		},
	})
	if err != nil {
		slog.Warn("component=discord", "msg", "server connect modal failed", "err", err.Error())
	}
}

// handleModalSubmit validates the submitted token against Nitrado, then
// stores it encrypted. The plaintext token is never logged, echoed, or kept
// beyond the lifetime of this function call.
func (h *ServerCommandHandler) handleModalSubmit(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.ModalSubmitData().CustomID != serverConnectModalID {
		return
	}
	if !isAdmin(s, i) {
		respondEphemeral(s, i, "⛔ You need Administrator or Manage Server permission to manage servers.")
		return
	}
	if h.cipher == nil {
		respondEphemeral(s, i, "❌ Server connection is unavailable: credential encryption is not configured.")
		return
	}
	token := strings.TrimSpace(modalValue(i.ModalSubmitData(), serverConnectTokenID))
	if token == "" {
		respondEphemeral(s, i, "❌ No token was provided.")
		return
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	}); err != nil {
		slog.Warn("component=discord", "msg", "server connect defer failed", "err", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, guildRowID, err := h.guilds.GetGuild(ctx, i.GuildID)
	if err != nil || guildRowID == 0 {
		h.editResponse(s, i, "❌ Run `/setup` before connecting a server.")
		return
	}

	client := nitrado.NewClient(nitrado.DefaultBaseURL, token, nil)
	if err := client.AuthenticationCheck(ctx); err != nil {
		h.editResponse(s, i, "❌ Token rejected by Nitrado: "+classifyNitradoErr(err))
		return
	}

	ciphertext, nonce, version, err := h.cipher.Encrypt([]byte(token))
	token = "" // never retain plaintext beyond this point
	if err != nil {
		h.editResponse(s, i, "❌ Could not securely store the token.")
		return
	}
	now := time.Now().UTC()
	conn := repository.NitradoConnection{
		GuildID:         guildRowID,
		Ciphertext:      ciphertext,
		Nonce:           nonce,
		KeyVersion:      version,
		Status:          "connected",
		LastValidatedAt: &now,
		LastSuccessAt:   &now,
	}
	if err := h.servers.SaveConnection(ctx, conn); err != nil {
		h.editResponse(s, i, "❌ Could not save the connection.")
		return
	}
	slog.Info("component=discord", "msg", "nitrado connection saved", "guild_id", i.GuildID)
	h.editResponse(s, i, "✅ Nitrado token verified and securely stored.\nRun `/server services` to see your DayZ servers, then `/server select`.")
}

// handleServices lists the real, DayZ-filtered services on the guild's
// connected Nitrado account.
func (h *ServerCommandHandler) handleServices(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	services, err := h.dayZServices(ctx, i.GuildID)
	if err != nil {
		respondEphemeral(s, i, "❌ "+err.Error())
		return
	}
	if len(services) == 0 {
		respondEphemeral(s, i, "No DayZ services were found on the connected Nitrado account.")
		return
	}
	var b strings.Builder
	b.WriteString("🏆 **DayZ Services Found**\n\n")
	for _, svc := range services {
		fmt.Fprintf(&b, "`%s` — %s (%s)\n", svc.ID, displayNameFromService(svc), svc.Status)
	}
	b.WriteString("\nRun `/server select` and choose a service to connect it.")
	respondEphemeral(s, i, b.String())
}

// handleSelect persists the chosen DayZ service as a game_servers row and
// starts its killfeed worker, seeded to begin at the current log tail so
// pre-existing log history is never replayed as new kills.
func (h *ServerCommandHandler) handleSelect(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	serviceID := strings.TrimSpace(optionString(sub, "service_id"))
	if serviceID == "" {
		respondEphemeral(s, i, "Provide a service ID (use the autocomplete list).")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	services, err := h.dayZServices(ctx, i.GuildID)
	if err != nil {
		respondEphemeral(s, i, "❌ "+err.Error())
		return
	}
	var matched *nitrado.Service
	for idx := range services {
		if services[idx].ID == serviceID {
			matched = &services[idx]
			break
		}
	}
	if matched == nil {
		respondEphemeral(s, i, "❌ That service ID was not found among your DayZ services. Run `/server services`.")
		return
	}

	_, guildRowID, err := h.guilds.GetGuild(ctx, i.GuildID)
	if err != nil || guildRowID == 0 {
		respondEphemeral(s, i, "❌ Run `/setup` before connecting a server.")
		return
	}

	row, err := h.servers.UpsertGameServer(ctx, repository.GameServer{
		GuildID:           guildRowID,
		Provider:          "NITRADO",
		ProviderServiceID: matched.ID,
		Game:              matched.Game,
		Platform:          platformFromService(*matched),
		DisplayName:       displayNameFromService(*matched),
		Status:            "CONNECTED",
		Active:            true,
	})
	if err != nil {
		respondEphemeral(s, i, "❌ Could not save the server.")
		return
	}
	if _, err := h.servers.EnsureConfig(ctx, row.ID); err != nil {
		slog.Warn("component=discord", "msg", "ensure server config failed", "server_id", row.ID, "err", err.Error())
	}

	if h.runtime == nil {
		respondEphemeral(s, i, fmt.Sprintf("✅ **%s** saved, but the killfeed runtime is not available (database required).", row.DisplayName))
		return
	}
	if err := h.runtime.ConnectServer(ctx, row.ID); err != nil {
		respondEphemeral(s, i, fmt.Sprintf("✅ **%s** saved, but the killfeed worker could not start: %s\nTry `/server repair`.", row.DisplayName, err.Error()))
		return
	}
	respondEphemeral(s, i, fmt.Sprintf("✅ **%s** connected and the killfeed worker has started.\nNew kills will begin appearing shortly — existing log history is skipped.", row.DisplayName))
}

// handleDisconnect stops the worker and deactivates the row, preserving all
// recorded history for later reconnection.
func (h *ServerCommandHandler) handleDisconnect(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	serviceID := strings.TrimSpace(optionString(sub, "service_id"))
	if serviceID == "" {
		respondEphemeral(s, i, "Provide a service ID.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, guildRowID, err := h.guilds.GetGuild(ctx, i.GuildID)
	if err != nil || guildRowID == 0 {
		respondEphemeral(s, i, "❌ Run `/setup` first.")
		return
	}
	row, err := h.servers.FindByGuildAndService(ctx, guildRowID, serviceID)
	if err != nil || row == nil {
		respondEphemeral(s, i, "❌ That server is not connected to this guild.")
		return
	}
	if err := h.servers.Deactivate(ctx, row.ID); err != nil {
		respondEphemeral(s, i, "❌ Could not disconnect the server.")
		return
	}
	if h.runtime != nil {
		h.runtime.DisconnectServer(row.ID)
	}
	respondEphemeral(s, i, fmt.Sprintf("✅ **%s** disconnected. History is preserved — run `/server select` to reconnect.", row.DisplayName))
}

// handleRepair re-validates the stored connection and reattaches the worker
// for an already-known server, without resetting its checkpoint to the tail
// (unlike a fresh /server select, a repair must not skip missed activity).
func (h *ServerCommandHandler) handleRepair(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	serviceID := strings.TrimSpace(optionString(sub, "service_id"))
	if serviceID == "" {
		respondEphemeral(s, i, "Provide a service ID.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, guildRowID, err := h.guilds.GetGuild(ctx, i.GuildID)
	if err != nil || guildRowID == 0 {
		respondEphemeral(s, i, "❌ Run `/setup` first.")
		return
	}
	row, err := h.servers.FindByGuildAndService(ctx, guildRowID, serviceID)
	if err != nil || row == nil {
		respondEphemeral(s, i, "❌ That server is not known to this guild. Run `/server select` instead.")
		return
	}

	services, err := h.dayZServices(ctx, i.GuildID)
	if err != nil {
		respondEphemeral(s, i, "❌ Re-validation failed: "+err.Error())
		return
	}
	found := false
	for _, svc := range services {
		if svc.ID == serviceID {
			found = true
			break
		}
	}
	if !found {
		respondEphemeral(s, i, "❌ That service was not found on the connected Nitrado account anymore.")
		return
	}

	if err := h.servers.Reactivate(ctx, row.ID); err != nil {
		respondEphemeral(s, i, "❌ Could not reactivate the server.")
		return
	}
	if h.runtime == nil {
		respondEphemeral(s, i, fmt.Sprintf("✅ **%s** re-validated, but the killfeed runtime is not available (database required).", row.DisplayName))
		return
	}
	if err := h.runtime.RepairServer(ctx, row.ID); err != nil {
		respondEphemeral(s, i, fmt.Sprintf("❌ Re-validated, but the worker could not be reattached: %s", err.Error()))
		return
	}
	respondEphemeral(s, i, fmt.Sprintf("✅ **%s** re-validated and its killfeed worker has been reattached.", row.DisplayName))
}

func (h *ServerCommandHandler) handleStatus(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx := context.Background()
	_, gid, err := h.guilds.GetGuild(ctx, i.GuildID)
	if err != nil || gid == 0 {
		respondEphemeral(s, i, "Run `/setup` first.")
		return
	}
	rows, err := h.servers.ListActiveByGuild(ctx, gid)
	if err != nil {
		respondEphemeral(s, i, "Could not load server connections.")
		return
	}
	var b strings.Builder
	b.WriteString("🏆 **CHAMPION SERVER CONNECTIONS**\n\n")
	for _, row := range rows {
		fmt.Fprintf(&b, "**%s**\n%s • %s\nStatus: %s\n\n", row.DisplayName, row.Game, row.Platform, row.Status)
	}
	if len(rows) == 0 {
		b.WriteString("No connected game server.\nRun `/server connect` to get started.")
	}
	respondEphemeral(s, i, b.String())
}

// handleAutocomplete offers a live, DayZ-filtered list of service IDs for the
// select/disconnect/repair subcommands.
func (h *ServerCommandHandler) handleAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	if len(data.Options) == 0 {
		respondAutocomplete(s, i, nil)
		return
	}
	sub := data.Options[0]
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	services, err := h.dayZServices(ctx, i.GuildID)
	if err != nil {
		respondAutocomplete(s, i, nil)
		return
	}
	typed := strings.ToLower(optionString(sub, "service_id"))
	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(services))
	for _, svc := range services {
		label := fmt.Sprintf("%s (%s) — %s", displayNameFromService(svc), svc.ID, svc.Status)
		if typed != "" && !strings.Contains(strings.ToLower(label), typed) {
			continue
		}
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: truncateLabel(label, 100), Value: svc.ID})
		if len(choices) >= 25 {
			break
		}
	}
	respondAutocomplete(s, i, choices)
}

// dayZServices resolves the guild's stored Nitrado connection, decrypts the
// token, and returns the live, DayZ-filtered service list. The plaintext
// token never leaves this function.
func (h *ServerCommandHandler) dayZServices(ctx context.Context, discordGuildID string) ([]nitrado.Service, error) {
	_, guildRowID, err := h.guilds.GetGuild(ctx, discordGuildID)
	if err != nil || guildRowID == 0 {
		return nil, fmt.Errorf("run `/setup` first")
	}
	if h.cipher == nil {
		return nil, fmt.Errorf("credential encryption is not configured")
	}
	conn, err := h.servers.GetConnection(ctx, guildRowID)
	if err != nil || conn == nil {
		return nil, fmt.Errorf("no Nitrado connection; run `/server connect` first")
	}
	plaintext, err := h.cipher.Decrypt(conn.Ciphertext, conn.Nonce, conn.KeyVersion)
	if err != nil {
		return nil, fmt.Errorf("stored Nitrado token could not be decrypted; run `/server connect` again")
	}
	client := nitrado.NewClient(nitrado.DefaultBaseURL, string(plaintext), nil)
	services, err := client.GetServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("Nitrado request failed: %s", classifyNitradoErr(err))
	}
	return nitrado.FindDayZServices(services), nil
}

func (h *ServerCommandHandler) editResponse(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
		slog.Warn("component=discord", "msg", "server command response edit failed", "err", err.Error())
	}
}

func respondAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate, choices []*discordgo.ApplicationCommandOptionChoice) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	})
}

// modalValue extracts a TextInput's submitted value by custom ID.
func modalValue(data discordgo.ModalSubmitInteractionData, customID string) string {
	for _, row := range data.Components {
		actionsRow, ok := row.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, comp := range actionsRow.Components {
			if input, ok := comp.(*discordgo.TextInput); ok && input.CustomID == customID {
				return input.Value
			}
		}
	}
	return ""
}

// classifyNitradoErr turns a Nitrado request failure into a safe, generic
// message. It never includes tokens, headers, or raw response bodies.
func classifyNitradoErr(err error) string {
	var reqErr *nitrado.RequestError
	if errors.As(err, &reqErr) {
		switch reqErr.Kind {
		case nitrado.KindAuthentication:
			return "invalid or expired token"
		case nitrado.KindPermission:
			return "token does not have permission for this request"
		case nitrado.KindNotFound:
			return "not found"
		case nitrado.KindTemporary:
			if reqErr.StatusCode == 429 {
				return "rate limited by Nitrado; try again shortly"
			}
			return "Nitrado is temporarily unavailable"
		default:
			return "unexpected Nitrado response"
		}
	}
	return "could not reach Nitrado"
}

// displayNameFromService picks the best available human-readable server name.
func displayNameFromService(svc nitrado.Service) string {
	if svc.Details.ServerName != "" {
		return svc.Details.ServerName
	}
	if svc.Details.Name != "" {
		return svc.Details.Name
	}
	if svc.Game != "" {
		return svc.Game
	}
	return "DayZ Server"
}

// platformFromService returns the deployment platform for a connected server.
// Nitrado's service payload does not reliably expose a platform field in this
// codebase's verified integrations, so this follows the same convention
// already used by the repository's legacy backfill migration (PLAYSTATION).
// Unverified/undocumented: a per-service platform field from the Nitrado API.
func platformFromService(nitrado.Service) string {
	return "PLAYSTATION"
}

func truncateLabel(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
