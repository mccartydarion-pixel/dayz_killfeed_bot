package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/factionhub"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

// Faction recruitment (docs/FACTIONS.md "Recruitment"). A faction leader or officer publishes
// one recruitment card into the installation's FACTION_RECRUITMENT channel from the website. The
// card carries the faction's design (colours, logo, flag, armband, description, requirements)
// and one button: Join on an OPEN faction (instant membership), Apply on an INVITE_ONLY one
// (an application for the leader to review). The card is edited in place when the faction
// changes and deleted when the faction is dissolved. Every Discord press is scoped to the
// guild it was pressed in: a faction id from another server is simply not found.

const (
	factionRecruitPrefix      = "champion:faction:"
	factionRecruitJoin        = factionRecruitPrefix + "join:"
	factionRecruitApply       = factionRecruitPrefix + "apply:"
	factionRecruitApplyModal  = factionRecruitPrefix + "applymodal:"
	factionRecruitMessageMax  = 500
	factionRecruitDescription = 600
)

// recruitMessageAPI is the slice of Discord the recruitment card needs (discord.SessionAPI, or a
// fake in tests).
type recruitMessageAPI interface {
	ChannelMessageSendComplex(channelID string, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) (*discordgo.Message, error)
	ChannelMessageEditComplex(channelID, messageID string, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) (*discordgo.Message, error)
	ChannelMessageDelete(channelID, messageID string) error
}

// Typed errors the handlers map to fixed responses.
var (
	errRecruitChannelUnset = errors.New("no faction recruitment channel is routed for this server")
	errRecruitUnavailable  = errors.New("faction recruitment is unavailable")
)

// siteURL is the public website origin (links on the card).
func (a *App) siteURL() string {
	if a != nil && a.Config != nil && a.Config.SiteBaseURL != "" {
		return strings.TrimRight(a.Config.SiteBaseURL, "/")
	}
	return billing.DefaultOrigin
}

// hexColor parses "#RRGGBB" into a Discord embed colour, falling back to the faction gold.
func hexColor(raw *string) int {
	if raw == nil || len(*raw) != 7 || (*raw)[0] != '#' {
		return presentation.FactionGold
	}
	v, err := strconv.ParseUint((*raw)[1:], 16, 32)
	if err != nil {
		return presentation.FactionGold
	}
	return int(v)
}

func titleCase(key string) string {
	if key == "" {
		return ""
	}
	return key[:1] + strings.ToLower(key[1:])
}

// BuildFactionRecruitCard renders the recruitment embed and its button row for a faction.
// It is pure so tests can pin the layout; `logoURL` is the public logo URL or "".
func BuildFactionRecruitCard(f repository.HubFaction, leader *repository.HubMember, logoURL, siteURL string) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	status := "Recruiting · press **Join** to join instantly"
	switch f.RecruitmentStatus {
	case factionhub.RecruitmentInviteOnly:
		status = "Invite only · press **Apply** and the leader will review your request"
	case factionhub.RecruitmentClosed:
		status = "Not recruiting right now"
	}
	description := strings.TrimSpace(f.Description)
	if description == "" {
		description = "_No description yet._"
	} else if len(description) > factionRecruitDescription {
		description = description[:factionRecruitDescription-1] + "…"
	}
	fields := []*discordgo.MessageEmbedField{
		{Name: "Members", Value: strconv.Itoa(f.MemberCount), Inline: true},
		{Name: "Recruitment", Value: titleCase(strings.ReplaceAll(f.RecruitmentStatus, "_", " ")), Inline: true},
	}
	if leader != nil {
		name := leader.User.GlobalName
		if name == "" {
			name = leader.User.Username
		}
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Leader", Value: name, Inline: true})
	}
	if f.FlagKey != nil || f.ArmbandKey != nil {
		parts := []string{}
		if f.FlagKey != nil {
			if cn, ok := factionhub.FlagClassName(*f.FlagKey); ok {
				parts = append(parts, "🏴 "+strings.TrimPrefix(cn, "Flag_"))
			}
		}
		if f.ArmbandKey != nil {
			parts = append(parts, "🎗️ "+titleCase(*f.ArmbandKey)+" armband")
		}
		fields = append(fields, &discordgo.MessageEmbedField{Name: "In game", Value: strings.Join(parts, " · "), Inline: false})
	}
	if req := requirementsLine(f.Settings); req != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Requirements", Value: req, Inline: false})
	}
	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("[%s] %s", f.Tag, f.Name),
		URL:         fmt.Sprintf("%s/dashboard/player/factions/%d", siteURL, f.ID),
		Description: description + "\n\n" + status,
		Color:       hexColor(f.PrimaryColor),
		Fields:      fields,
		Footer:      &discordgo.MessageEmbedFooter{Text: "Champion Factions · " + strings.TrimPrefix(siteURL, "https://")},
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
	if logoURL != "" {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: logoURL}
	}
	id := strconv.FormatInt(f.ID, 10)
	buttons := []discordgo.MessageComponent{}
	switch f.RecruitmentStatus {
	case factionhub.RecruitmentOpen:
		buttons = append(buttons, discordgo.Button{CustomID: factionRecruitJoin + id, Label: "Join " + f.Tag, Style: discordgo.SuccessButton, Emoji: &discordgo.ComponentEmoji{Name: "⚔️"}})
	case factionhub.RecruitmentInviteOnly:
		buttons = append(buttons, discordgo.Button{CustomID: factionRecruitApply + id, Label: "Apply to " + f.Tag, Style: discordgo.PrimaryButton, Emoji: &discordgo.ComponentEmoji{Name: "📝"}})
	}
	buttons = append(buttons, discordgo.Button{Label: "View on Champion", Style: discordgo.LinkButton, URL: embed.URL})
	return embed, []discordgo.MessageComponent{discordgo.ActionsRow{Components: buttons}}
}

func requirementsLine(s factionhub.Settings) string {
	parts := []string{}
	if s.MinimumHours != nil && *s.MinimumHours > 0 {
		parts = append(parts, fmt.Sprintf("%d+ hours", *s.MinimumHours))
	}
	if s.MinimumAge != nil && *s.MinimumAge > 0 {
		parts = append(parts, fmt.Sprintf("%d+ years old", *s.MinimumAge))
	}
	if s.PvPRequired {
		parts = append(parts, "PvP")
	}
	if s.MicRequired {
		parts = append(parts, "Mic")
	}
	if s.BuilderNeeded {
		parts = append(parts, "Builders wanted")
	}
	if strings.TrimSpace(s.CustomRequirements) != "" {
		c := strings.TrimSpace(s.CustomRequirements)
		if len(c) > 200 {
			c = c[:199] + "…"
		}
		parts = append(parts, c)
	}
	return strings.Join(parts, " · ")
}

// recruitAPI returns the Discord message API for cards, or nil when the bot is not connected.
func (a *App) recruitAPI() recruitMessageAPI {
	if a.factionRecruitAPI != nil {
		return a.factionRecruitAPI
	}
	return nil
}

// recruitChannel resolves the FACTION_RECRUITMENT channel for the faction's installation.
func (a *App) recruitChannel(ctx context.Context, f repository.HubFaction) (string, error) {
	if a.ChannelRoutes == nil || a.SaaSInstallations == nil || a.SaaSGuildConnections == nil {
		return "", errRecruitUnavailable
	}
	inst, err := a.SaaSInstallations.GetScoped(ctx, f.OrganizationID, f.InstallationID)
	if err != nil || inst == nil {
		return "", errRecruitUnavailable
	}
	conn, err := a.SaaSGuildConnections.GetScoped(ctx, f.OrganizationID, inst.DiscordGuildConnectionID)
	if err != nil || conn == nil {
		return "", errRecruitUnavailable
	}
	channelID, found, err := a.ChannelRoutes.Resolve(ctx, conn.GuildID, f.GameServerID, routing.RouteFactionRecruitment)
	if err != nil {
		return "", fmt.Errorf("resolve recruitment channel: %w", err)
	}
	if !found || channelID == "" {
		return "", errRecruitChannelUnset
	}
	return channelID, nil
}

func (a *App) recruitCard(ctx context.Context, f repository.HubFaction) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	leader, err := a.FactionHub.LeaderOf(ctx, f.ID)
	if err != nil {
		slog.Warn("component=faction_recruit", "msg", "leader lookup failed", "err", err.Error())
	}
	logoURL := ""
	if dto := toFactionLogo(f.Logo, a.assetBaseURL()); dto != nil {
		logoURL = dto.URL
	}
	return BuildFactionRecruitCard(f, leader, logoURL, a.siteURL())
}

// publishFactionRecruit posts the faction's card, or edits the existing one in place; a card
// whose message was deleted in Discord is posted again.
func (a *App) publishFactionRecruit(ctx context.Context, f repository.HubFaction, actorUserID *int64) (*repository.HubRecruitPost, error) {
	api := a.recruitAPI()
	if api == nil {
		return nil, errRecruitUnavailable
	}
	channelID, err := a.recruitChannel(ctx, f)
	if err != nil {
		return nil, err
	}
	embed, components := a.recruitCard(ctx, f)
	existing, err := a.FactionHub.RecruitPost(ctx, f.InstallationID, f.ID)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.ChannelID == channelID {
		if _, err := api.ChannelMessageEditComplex(existing.ChannelID, existing.MessageID, embed, components); err == nil {
			_ = a.FactionHub.TouchRecruitPost(ctx, f.ID)
			return a.FactionHub.RecruitPost(ctx, f.InstallationID, f.ID)
		} else if !isUnknownDiscordMessage(err) {
			return nil, fmt.Errorf("edit recruitment card: %w", err)
		}
	} else if existing != nil {
		// The route moved to another channel: retire the old card.
		_ = api.ChannelMessageDelete(existing.ChannelID, existing.MessageID)
	}
	msg, err := api.ChannelMessageSendComplex(channelID, embed, components)
	if err != nil {
		return nil, fmt.Errorf("post recruitment card: %w", err)
	}
	if err := a.FactionHub.UpsertRecruitPost(ctx, f.InstallationID, f.ID, channelID, msg.ID, actorUserID); err != nil {
		_ = api.ChannelMessageDelete(channelID, msg.ID)
		return nil, err
	}
	return a.FactionHub.RecruitPost(ctx, f.InstallationID, f.ID)
}

// refreshFactionRecruit re-renders an existing card after the faction changed. Best effort:
// it never fails the change that triggered it. A card whose message is gone is forgotten.
func (a *App) refreshFactionRecruit(ctx context.Context, organizationID, installationID, factionID int64) {
	api := a.recruitAPI()
	if api == nil || a.FactionHub == nil {
		return
	}
	post, err := a.FactionHub.RecruitPost(ctx, installationID, factionID)
	if err != nil || post == nil {
		return
	}
	f, err := a.FactionHub.Get(ctx, organizationID, installationID, factionID)
	if err != nil || f == nil {
		return
	}
	embed, components := a.recruitCard(ctx, *f)
	if _, err := api.ChannelMessageEditComplex(post.ChannelID, post.MessageID, embed, components); err != nil {
		if isUnknownDiscordMessage(err) {
			_ = a.FactionHub.DeleteRecruitPost(ctx, factionID)
			return
		}
		slog.Warn("component=faction_recruit", "msg", "card refresh failed", "faction_id", factionID, "err", err.Error())
		return
	}
	_ = a.FactionHub.TouchRecruitPost(ctx, factionID)
}

// removeFactionRecruit deletes the card and forgets it (unpublish, or dissolve).
func (a *App) removeFactionRecruit(ctx context.Context, installationID, factionID int64) error {
	post, err := a.FactionHub.RecruitPost(ctx, installationID, factionID)
	if err != nil || post == nil {
		return err
	}
	if api := a.recruitAPI(); api != nil {
		if err := api.ChannelMessageDelete(post.ChannelID, post.MessageID); err != nil && !isUnknownDiscordMessage(err) {
			return fmt.Errorf("delete recruitment card: %w", err)
		}
	}
	return a.FactionHub.DeleteRecruitPost(ctx, factionID)
}

func isUnknownDiscordMessage(err error) bool {
	var rest *discordgo.RESTError
	if !errors.As(err, &rest) {
		return false
	}
	if rest.Message != nil && (rest.Message.Code == discordgo.ErrCodeUnknownMessage || rest.Message.Code == discordgo.ErrCodeUnknownChannel) {
		return true
	}
	return rest.Response != nil && rest.Response.StatusCode == http.StatusNotFound
}

// --- HTTP -------------------------------------------------------------------------------------

type recruitPostDTO struct {
	ChannelID string  `json:"channelId"`
	MessageID string  `json:"messageId"`
	URL       string  `json:"url"`
	PostedAt  string  `json:"postedAt"`
	UpdatedAt string  `json:"updatedAt"`
	Channel   *string `json:"channel,omitempty"`
}

func (a *App) toRecruitPost(ctx context.Context, f repository.HubFaction, p *repository.HubRecruitPost) *recruitPostDTO {
	if p == nil {
		return nil
	}
	guild := ""
	if a.SaaSInstallations != nil && a.SaaSGuildConnections != nil {
		if inst, err := a.SaaSInstallations.GetScoped(ctx, f.OrganizationID, f.InstallationID); err == nil && inst != nil {
			if conn, err := a.SaaSGuildConnections.GetScoped(ctx, f.OrganizationID, inst.DiscordGuildConnectionID); err == nil && conn != nil {
				if g, err := a.Guilds.GetGuildByID(ctx, conn.GuildID); err == nil && g != nil {
					guild = g.DiscordGuildID
				}
			}
		}
	}
	url := ""
	if guild != "" {
		url = fmt.Sprintf("https://discord.com/channels/%s/%s/%s", guild, p.ChannelID, p.MessageID)
	}
	return &recruitPostDTO{ChannelID: p.ChannelID, MessageID: p.MessageID, URL: url, PostedAt: p.PostedAt.UTC().Format(time.RFC3339), UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339)}
}

func (a *App) registerFactionRecruitRoutes(base string) {
	h := a.HTTPServer.Handle
	h("POST "+base+"/{factionID}/join", a.handleJoinFaction)
	h("POST "+base+"/{factionID}/recruit", a.handlePublishRecruit)
	h("DELETE "+base+"/{factionID}/recruit", a.handleUnpublishRecruit)
}

// handleJoinFaction is POST .../factions/{factionID}/join: instant membership of an OPEN faction.
func (a *App) handleJoinFaction(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	factionID, ok := pathInt64(w, r, "factionID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	member, err := a.FactionHub.JoinFaction(ctx, fr.orgID, fr.instID, factionID, fr.user.ID)
	if err != nil {
		factionFailed(w, "join faction", err)
		return
	}
	factionAudit("faction_member_joined", fr, "faction_id", factionID, "member_id", member.ID)
	a.factionStatsChanged(fr, factionID)
	writeSaaSJSON(w, http.StatusCreated, map[string]any{"joined": true, "member": toFactionMember(*member)})
}

// handlePublishRecruit is POST .../factions/{factionID}/recruit (LEADER or OFFICER): post or
// refresh the faction's recruitment card.
func (a *App) handlePublishRecruit(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	factionID, ok := pathInt64(w, r, "factionID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	role, err := a.FactionHub.MemberRole(ctx, fr.orgID, fr.instID, factionID, fr.user.ID)
	if err != nil {
		factionFailed(w, "publish recruitment", err)
		return
	}
	if !factionhub.CanManageApplications(role) {
		writeSaaSError(w, codeForbidden, "only the leader or an officer can publish the recruitment card")
		return
	}
	f, err := a.FactionHub.Get(ctx, fr.orgID, fr.instID, factionID)
	if err != nil {
		factionFailed(w, "publish recruitment", err)
		return
	}
	actor := fr.user.ID
	post, err := a.publishFactionRecruit(ctx, *f, &actor)
	if err != nil {
		switch {
		case errors.Is(err, errRecruitChannelUnset):
			writeSaaSError(w, codeConflict, "your server owner has not routed a Faction Recruitment channel yet")
		case errors.Is(err, errRecruitUnavailable):
			writeSaaSError(w, codeInternalError, "faction recruitment is unavailable right now")
		default:
			slog.Warn("component=faction_recruit", "msg", "publish failed", "faction_id", factionID, "err", err.Error())
			writeSaaSError(w, codeInternalError, "could not post the recruitment card")
		}
		return
	}
	factionAudit("faction_recruit_published", fr, "faction_id", factionID)
	writeSaaSJSON(w, http.StatusOK, map[string]any{"recruitPost": a.toRecruitPost(ctx, *f, post)})
}

// handleUnpublishRecruit is DELETE .../factions/{factionID}/recruit (LEADER or OFFICER).
func (a *App) handleUnpublishRecruit(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	factionID, ok := pathInt64(w, r, "factionID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	role, err := a.FactionHub.MemberRole(ctx, fr.orgID, fr.instID, factionID, fr.user.ID)
	if err != nil {
		factionFailed(w, "unpublish recruitment", err)
		return
	}
	if !factionhub.CanManageApplications(role) {
		writeSaaSError(w, codeForbidden, "only the leader or an officer can remove the recruitment card")
		return
	}
	if err := a.removeFactionRecruit(ctx, fr.instID, factionID); err != nil {
		slog.Warn("component=faction_recruit", "msg", "unpublish failed", "faction_id", factionID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not remove the recruitment card")
		return
	}
	factionAudit("faction_recruit_unpublished", fr, "faction_id", factionID)
	writeSaaSJSON(w, http.StatusOK, map[string]any{"recruitPost": nil})
}

// --- Discord ----------------------------------------------------------------------------------

// IsFactionRecruitInteraction reports whether an interaction belongs to the recruitment card.
func IsFactionRecruitInteraction(customID string) bool {
	return strings.HasPrefix(customID, factionRecruitPrefix)
}

// HandleFactionRecruitInteraction handles Join / Apply presses and the Apply modal.
func (a *App) HandleFactionRecruitInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i == nil || i.GuildID == "" || i.Member == nil || i.Member.User == nil {
		return
	}
	reply := func(text string) { discord.RespondEphemeral(s, i, text) }
	var customID string
	switch i.Type {
	case discordgo.InteractionMessageComponent:
		customID = i.MessageComponentData().CustomID
	case discordgo.InteractionModalSubmit:
		customID = i.ModalSubmitData().CustomID
	default:
		return
	}
	action, rawID := splitRecruitCustomID(customID)
	timeout := factionTimeout
	if action == "apply" {
		// Apply opens a form, which Discord only accepts as the first answer
		// and cannot defer: the lookups before it get a short budget.
		timeout = modalLookupBudget
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	factionID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || factionID <= 0 || a.FactionHub == nil || a.Guilds == nil || a.SaaSUsers == nil {
		reply("This button is no longer valid.")
		return
	}
	_, guildRowID, err := a.Guilds.GetGuild(ctx, i.GuildID)
	if ctx.Err() != nil {
		reply("Something went wrong on our side. Try again in a moment.")
		return
	}
	if err != nil || guildRowID == 0 {
		reply("Champion isn't set up on this server yet.")
		return
	}
	f, err := a.FactionHub.FactionForGuild(ctx, guildRowID, factionID)
	if ctx.Err() != nil {
		reply("Something went wrong on our side. Try again in a moment.")
		return
	}
	if err != nil {
		reply("That faction no longer exists.")
		return
	}
	u := i.Member.User
	user, err := a.SaaSUsers.EnsureDiscordUser(ctx, u.ID, u.Username, u.GlobalName, u.Avatar)
	if err != nil {
		reply("Something went wrong on our side. Try again in a moment.")
		return
	}
	site := a.siteURL()
	switch action {
	case "join":
		member, err := a.FactionHub.JoinFaction(ctx, f.OrganizationID, f.InstallationID, f.ID, user.ID)
		if err != nil {
			reply(recruitFailureText(err, f, site))
			return
		}
		// Answer first: updating the recruit card is a rate-limited Discord edit.
		reply(fmt.Sprintf("⚔️ Welcome to **%s** [%s]! You're in as %s. Manage your faction life at %s/dashboard/player/factions/mine", f.Name, f.Tag, titleCase(member.RoleKey), site))
		if a.FactionHubStats != nil {
			a.FactionHubStats.Invalidate(f.OrganizationID, f.InstallationID, f.ID)
		}
		a.refreshFactionRecruit(ctx, f.OrganizationID, f.InstallationID, f.ID)
	case "apply":
		// Ask for a short message first; the application is created on modal submit.
		_ = discord.RespondModal(s, i, &discordgo.InteractionResponseData{
			CustomID: factionRecruitApplyModal + rawID,
			Title:    "Apply to " + f.Name,
			Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.TextInput{CustomID: "message", Label: "Tell the leader about yourself (optional)", Style: discordgo.TextInputParagraph, Placeholder: "Hours played, timezone, how you like to play…", Required: false, MaxLength: factionRecruitMessageMax},
			}}},
		})
	case "applymodal":
		message := ""
		for _, row := range i.ModalSubmitData().Components {
			if ar, ok := row.(*discordgo.ActionsRow); ok {
				for _, c := range ar.Components {
					if ti, ok := c.(*discordgo.TextInput); ok && ti.CustomID == "message" {
						message = ti.Value
					}
				}
			}
		}
		if _, err := factionhub.ValidateMessage(message); err != nil {
			message = ""
		}
		if _, err := a.FactionHub.Apply(ctx, f.OrganizationID, f.InstallationID, f.ID, user.ID, message); err != nil {
			reply(recruitFailureText(err, f, site))
			return
		}
		reply(fmt.Sprintf("📝 Your application to **%s** [%s] is in. The leader will review it; you'll find its status at %s/dashboard/player/factions/applications", f.Name, f.Tag, site))
	default:
		reply("This button is no longer valid.")
	}
}

func splitRecruitCustomID(customID string) (action, id string) {
	rest := strings.TrimPrefix(customID, factionRecruitPrefix)
	action, id, _ = strings.Cut(rest, ":")
	return action, id
}

func recruitFailureText(err error, f *repository.HubFaction, site string) string {
	switch {
	case errors.Is(err, factionhub.ErrAlreadyInFaction):
		return "You're already in a faction on this server. Leave it first at " + site + "/dashboard/player/factions/mine"
	case errors.Is(err, factionhub.ErrAlreadyApplied):
		return "You already have a pending application to " + f.Name + ". The leader will get to it."
	case errors.Is(err, factionhub.ErrJoinRequiresOpen):
		return f.Name + " is invite only now. Use **Apply** instead."
	case errors.Is(err, factionhub.ErrRecruitmentClosed):
		return f.Name + " isn't recruiting right now."
	case errors.Is(err, factionhub.ErrNoServer), errors.Is(err, factionhub.ErrInstallationInert):
		return "This server's Champion setup isn't finished, so factions are paused."
	default:
		var invalid *factionhub.ValidationError
		if errors.As(err, &invalid) {
			return "That didn't go through: " + strings.Join(invalid.Issues, "; ")
		}
		return "Something went wrong on our side. Try again in a moment."
	}
}
