package discord

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/admin"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

type AdminCommandHandler struct{ service *admin.Service }

func NewAdminCommandHandler(s *admin.Service) *AdminCommandHandler {
	return &AdminCommandHandler{service: s}
}
func RegisterAdminCommands(session *discordgo.Session, guildID string) error {
	applicationID, err := ApplicationID(session)
	if err != nil {
		return err
	}
	perms := int64(discordgo.PermissionAdministrator | discordgo.PermissionManageServer)
	cmd := &discordgo.ApplicationCommand{Name: "admin", Description: "Champion operations and diagnostics", DefaultMemberPermissions: &perms, Options: []*discordgo.ApplicationCommandOption{{Name: "status", Description: "Show system status", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "diagnostics", Description: "Show sanitized diagnostics", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "link-diagnostics", Description: "Show account-link readiness", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "presence-diagnostics", Description: "Show live presence and voice counter state", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "pipeline-diagnostics", Description: "Show live pipeline state", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "pipeline-reset-diagnostics", Description: "Clear live pipeline diagnostics", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "leaderboard-refresh", Description: "Manually refresh the public leaderboard panel", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "pipeline", Description: "Show kill pipeline state", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "workers", Description: "Show worker state", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "permissions", Description: "Show Discord permission state", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "health", Description: "Show component health", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "logs", Description: "Show recent operational state", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "checkpoint", Description: "Show checkpoint state", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "resync", Description: "Refresh safe runtime state", Type: discordgo.ApplicationCommandOptionSubCommand}}}
	_, err = session.ApplicationCommandCreate(applicationID, guildID, cmd)
	return err
}
func (h *AdminCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || h.service == nil || i == nil {
		respondEphemeral(s, i, "Admin diagnostics unavailable.")
		return
	}
	if !isAdminInteraction(i) {
		respondEphemeral(s, i, "Administrator or Manage Server permission required.")
		return
	}
	data := h.service.Status(context.Background())
	if len(i.ApplicationCommandData().Options) > 0 && i.ApplicationCommandData().Options[0].Name == "checkpoint" {
		runtime, _ := data["runtime"].(map[string]any)
		respondEphemeral(s, i, fmt.Sprintf("📍 **CHECKPOINT**\nLoaded: %v\nOffset: %v", runtime["checkpoint_loaded"], runtime["checkpoint_offset"]))
		return
	}
	if len(i.ApplicationCommandData().Options) > 0 && i.ApplicationCommandData().Options[0].Name == "resync" {
		respondEphemeral(s, i, "🏆 **RESYNC REQUESTED**\nSafe runtime refresh is scheduled; history and checkpoints are unchanged.")
		return
	}
	if i.ApplicationCommandData().Options[0].Name == "workers" {
		respondEphemeral(s, i, fmt.Sprintf("⚙️ **CHAMPION WORKERS**\n%v", data["workers"]))
		return
	}
	if i.ApplicationCommandData().Options[0].Name == "link-diagnostics" {
		respondLinkDiagnostics(s, i, data["link_diagnostics"])
		return
	}
	if i.ApplicationCommandData().Options[0].Name == "presence-diagnostics" {
		respondPresenceDiagnostics(s, i, data["presence_diagnostics"])
		return
	}
	if i.ApplicationCommandData().Options[0].Name == "pipeline-diagnostics" {
		respondPipelineDiagnostics(s, i, data["pipeline_diagnostics"])
		return
	}
	if i.ApplicationCommandData().Options[0].Name == "pipeline-reset-diagnostics" {
		respondEphemeral(s, i, "Pipeline diagnostics reset is available after the next runtime snapshot.")
		return
	}
	if i.ApplicationCommandData().Options[0].Name == "leaderboard-refresh" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := h.service.RefreshLeaderboard(ctx); err != nil {
			respondEphemeral(s, i, "❌ Leaderboard refresh failed: "+err.Error())
			return
		}
		respondEphemeral(s, i, "✅ Leaderboard refreshed.")
		return
	}
	runtime, _ := data["runtime"].(map[string]any)
	var b strings.Builder
	b.WriteString("🏆 **CHAMPION SYSTEM STATUS**\n\n")
	if runtime != nil {
		fmt.Fprintf(&b, "Database: %v\nDiscord: %v\nNitrado: %v\nADM: %v\nOnline Players: %v\nPersistence Queue: %v\n", runtime["database_connected"], runtime["discord_connected"], runtime["nitrado_authenticated"], runtime["log_source_found"], runtime["online_players"], runtime["persistence_queue_depth"])
	}
	respondEphemeral(s, i, b.String())
}

func respondPipelineDiagnostics(s *discordgo.Session, i *discordgo.InteractionCreate, raw any) {
	values, _ := raw.(map[string]any)
	embed := presentation.NewChampionEmbed("PIPELINE DIAGNOSTICS", presentation.InfoSteel)
	for _, field := range []string{"worker", "server_id", "selected_adm", "newest_adm", "selection_match", "selection_reason", "metadata", "download", "reader", "parser", "persistence", "checkpoint", "presence", "voice", "kill", "last_failure", "classification", "timeline"} {
		if value, ok := values[field]; ok {
			embed.Fields = append(embed.Fields, presentation.StatusField(strings.ToUpper(field), fmt.Sprint(value), false))
		}
	}
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Embeds: []*discordgo.MessageEmbed{embed}}})
}

func respondPresenceDiagnostics(s *discordgo.Session, i *discordgo.InteractionCreate, raw any) {
	embed := presentation.NewChampionEmbed("PRESENCE DIAGNOSTICS", presentation.InfoSteel)
	values, _ := raw.(map[string]any)
	add := func(label, key string) {
		if value, ok := values[key]; ok {
			embed.Fields = append(embed.Fields, presentation.StatusField(label, fmt.Sprint(value), true))
		}
	}
	for _, field := range [][2]string{{"SELECTED SERVER", "selected_server_id"}, {"SELECTED SERVER RESOLVED", "selected_server_id_resolved"}, {"WORKER", "selected_server_worker_found"}, {"TRACKER COUNT", "tracker_count"}, {"TRACKED ENTRIES", "tracked_entries"}, {"LAST PRESENCE EVENT", "last_presence_event"}, {"LAST CONNECT", "last_connect_at"}, {"LAST DISCONNECT", "last_disconnect_at"}, {"LAST PERSISTENCE", "last_persistence_result"}, {"VOICE COUNTER PUBLISHED", "last_voice_publish_count"}, {"VOICE PUBLISH RESULT", "last_voice_publish_result"}, {"DISCORD VOICE CHANNEL", "discord_voice_channel"}, {"DISCORD VOICE COUNTER", "discord_voice_counter"}, {"CLASSIFICATION", "classification"}} {
		add(field[0], field[1])
	}
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Embeds: []*discordgo.MessageEmbed{embed}}})
}

func respondLinkDiagnostics(s *discordgo.Session, i *discordgo.InteractionCreate, raw any) {
	embed := presentation.NewChampionEmbed("LINK DIAGNOSTICS", presentation.InfoSteel)
	values, _ := raw.(map[string]any)
	add := func(label, key string) {
		if value, ok := values[key]; ok {
			embed.Fields = append(embed.Fields, presentation.StatusField(label, fmt.Sprint(value), true))
		}
	}
	add("DATABASE", "database")
	add("PLAYER REPOSITORY", "player_repository")
	add("ACTIVITY REPOSITORY", "activity_repository")
	add("SELECTED SERVER", "selected_server")
	add("OBSERVED PLAYERS", "observed_players")
	add("LAST CONNECT PERSISTED", "last_player_connect_persisted")
	add("LAST DISCONNECT", "last_player_disconnect")
	add("PRESENCE EVENT", "last_presence_event")
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Embeds: []*discordgo.MessageEmbed{embed}}})
}
