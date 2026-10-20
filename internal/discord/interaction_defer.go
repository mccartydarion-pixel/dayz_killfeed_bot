package discord

import (
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Answering interactions. Discord shows "The application did not respond"
// unless an interaction is acknowledged within 3 s. Every acknowledgement in
// the bot goes through this file (a test fails on a direct InteractionRespond
// anywhere else), so each interaction has one state: not answered yet,
// answered, or deferred. The reply helpers send the answer when nothing was
// sent yet and otherwise fill in the deferred one, which lets the router (see
// interaction_router.go) defer a slow handler without the handler knowing.

// AckMode is how an interaction is acknowledged when its handler is slow.
type AckMode int

const (
	// AckPrivate defers with a "thinking..." reply only the user sees.
	AckPrivate AckMode = iota + 1
	// AckPublic defers with a "thinking..." reply the whole channel sees.
	AckPublic
	// AckUpdate defers a button or form that edits the message it sits on.
	AckUpdate
	// AckSelf is for the two answers Discord cannot defer: opening a form
	// (modal) and autocomplete suggestions. The handler must answer itself and
	// do no unbounded work before it does.
	AckSelf
)

func (m AckMode) String() string {
	switch m {
	case AckPrivate:
		return "private"
	case AckPublic:
		return "public"
	case AckUpdate:
		return "update"
	case AckSelf:
		return "self"
	}
	return "invalid"
}

// interactionFailureText (voice.go) answers an interaction whose handler crashed or
// returned without answering.

type replyState struct {
	mu       sync.Mutex
	at       time.Time
	answered bool    // an acknowledgement reached Discord
	deferred AckMode // the kind of deferral the interaction was acknowledged with, or 0
	filled   bool    // the deferral has been given its real reply
}

var deferredReplies sync.Map // interaction ID -> *replyState

func stateOf(i *discordgo.InteractionCreate) *replyState {
	if i == nil || i.Interaction == nil {
		return &replyState{}
	}
	if existing, ok := deferredReplies.Load(i.ID); ok {
		return existing.(*replyState)
	}
	now := time.Now()
	deferredReplies.Range(func(key, value any) bool {
		if now.Sub(value.(*replyState).at) > interactionTTL {
			deferredReplies.Delete(key)
		}
		return true
	})
	actual, _ := deferredReplies.LoadOrStore(i.ID, &replyState{at: now})
	return actual.(*replyState)
}

func usable(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	return s != nil && i != nil && i.Interaction != nil
}

// deferAs acknowledges the interaction with a deferral of the given kind. It
// reports true when the interaction is acknowledged (now or already).
func deferAs(s *discordgo.Session, i *discordgo.InteractionCreate, mode AckMode) bool {
	if !usable(s, i) {
		return false
	}
	st := stateOf(i)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.answered {
		return true
	}
	resp := &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredChannelMessageWithSource}
	switch mode {
	case AckPrivate:
		resp.Data = &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral}
	case AckPublic:
	case AckUpdate:
		resp.Type = discordgo.InteractionResponseDeferredMessageUpdate
	default:
		return false
	}
	if err := s.InteractionRespond(i.Interaction, resp); err != nil {
		return false
	}
	st.answered, st.deferred = true, mode
	return true
}

// deferEphemeral acknowledges the interaction with a private "thinking..."
// reply. It reports false if Discord refused (the interaction has expired).
func deferEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	return deferAs(s, i, AckPrivate)
}

// DeferEphemeral is the exported deferEphemeral for app-level handlers.
func DeferEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	return deferEphemeral(s, i)
}

// deferPublic acknowledges with a "thinking..." reply the channel can see.
func deferPublic(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	return deferAs(s, i, AckPublic)
}

func isDeferred(i *discordgo.InteractionCreate) bool {
	if i == nil || i.Interaction == nil {
		return false
	}
	existing, ok := deferredReplies.Load(i.ID)
	if !ok {
		return false
	}
	st := existing.(*replyState)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.deferred != 0
}

func isAnswered(i *discordgo.InteractionCreate) bool {
	if i == nil || i.Interaction == nil {
		return false
	}
	existing, ok := deferredReplies.Load(i.ID)
	if !ok {
		return false
	}
	st := existing.(*replyState)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.answered
}

func editOriginal(s *discordgo.Session, i *discordgo.InteractionCreate, data *discordgo.InteractionResponseData) {
	content := data.Content
	embeds := data.Embeds
	if embeds == nil {
		embeds = []*discordgo.MessageEmbed{}
	}
	edit := &discordgo.WebhookEdit{Content: &content, Embeds: &embeds, AllowedMentions: data.AllowedMentions}
	if data.Components != nil {
		components := data.Components
		edit.Components = &components
	}
	if _, err := s.InteractionResponseEdit(i.Interaction, edit); err != nil {
		slog.Warn("component=discord", "msg", "interaction reply edit failed", "interaction", interactionLabel(i))
	}
}

func followUpPrivate(s *discordgo.Session, i *discordgo.InteractionCreate, data *discordgo.InteractionResponseData) {
	_, err := s.FollowupMessageCreate(i.Interaction, false, &discordgo.WebhookParams{
		Content: data.Content, Embeds: data.Embeds, Components: data.Components,
		AllowedMentions: data.AllowedMentions, Flags: discordgo.MessageFlagsEphemeral,
	})
	if err != nil {
		slog.Warn("component=discord", "msg", "interaction follow-up failed", "interaction", interactionLabel(i))
	}
}

// editDeferred fills in a deferred reply with an edit the caller built (an
// embed, a file). Handlers use it instead of editing the response directly so
// the router knows the "thinking..." reply was replaced.
func editDeferred(s *discordgo.Session, i *discordgo.InteractionCreate, edit *discordgo.WebhookEdit) error {
	if !usable(s, i) || edit == nil {
		return nil
	}
	st := stateOf(i)
	st.mu.Lock()
	st.filled = true
	st.mu.Unlock()
	_, err := s.InteractionResponseEdit(i.Interaction, edit)
	return err
}

// respondPrivate sends the private reply, or fills in the deferred one. After a
// public or message-update deferral the reply still reaches only the user.
func respondPrivate(s *discordgo.Session, i *discordgo.InteractionCreate, data *discordgo.InteractionResponseData) {
	if !usable(s, i) || data == nil {
		return
	}
	st := stateOf(i)
	st.mu.Lock()
	defer st.mu.Unlock()
	switch {
	case st.deferred == AckPrivate:
		st.filled = true
		editOriginal(s, i, data)
	case st.deferred == AckPublic:
		st.filled = true
		// A deferred reply cannot change who sees it: remove the public
		// placeholder and answer privately instead.
		_ = s.InteractionResponseDelete(i.Interaction)
		followUpPrivate(s, i, data)
		st.deferred = AckUpdate // later private replies are follow-ups too
	case st.deferred == AckUpdate:
		st.filled = true
		followUpPrivate(s, i, data)
	case st.answered:
		// Already answered in full; Discord accepts one answer.
	default:
		data.Flags |= discordgo.MessageFlagsEphemeral
		if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: data}); err == nil {
			st.answered = true
		}
	}
}

// RespondPrivate is the exported respondPrivate for app-level handlers.
func RespondPrivate(s *discordgo.Session, i *discordgo.InteractionCreate, data *discordgo.InteractionResponseData) {
	respondPrivate(s, i, data)
}

// respondUpdate replaces the message a button or form belongs to.
func respondUpdate(s *discordgo.Session, i *discordgo.InteractionCreate, data *discordgo.InteractionResponseData) {
	if !usable(s, i) || data == nil {
		return
	}
	st := stateOf(i)
	st.mu.Lock()
	defer st.mu.Unlock()
	switch {
	case st.deferred != 0:
		st.filled = true
		// The same fields an immediate update sends: absent embeds and
		// components stay as they are on the message.
		content := data.Content
		edit := &discordgo.WebhookEdit{Content: &content, AllowedMentions: data.AllowedMentions}
		if data.Embeds != nil {
			embeds := data.Embeds
			edit.Embeds = &embeds
		}
		if data.Components != nil {
			components := data.Components
			edit.Components = &components
		}
		if _, err := s.InteractionResponseEdit(i.Interaction, edit); err != nil {
			slog.Warn("component=discord", "msg", "interaction message update failed", "interaction", interactionLabel(i))
		}
	case st.answered:
	default:
		if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseUpdateMessage, Data: data}); err == nil {
			st.answered = true
		}
	}
}

// RespondUpdate is the exported respondUpdate for app-level handlers.
func RespondUpdate(s *discordgo.Session, i *discordgo.InteractionCreate, data *discordgo.InteractionResponseData) {
	respondUpdate(s, i, data)
}

// respondModalData opens a form. Discord only accepts a form as the first
// answer, so when the interaction was already acknowledged the user is asked
// to try again.
func respondModalData(s *discordgo.Session, i *discordgo.InteractionCreate, data *discordgo.InteractionResponseData) error {
	if !usable(s, i) || data == nil {
		return nil
	}
	st := stateOf(i)
	st.mu.Lock()
	if st.answered {
		st.mu.Unlock()
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: interactionFailureText})
		return nil
	}
	defer st.mu.Unlock()
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseModal, Data: data})
	if err == nil {
		st.answered = true
	}
	return err
}

// RespondModal is the exported respondModalData for app-level handlers.
func RespondModal(s *discordgo.Session, i *discordgo.InteractionCreate, data *discordgo.InteractionResponseData) error {
	return respondModalData(s, i, data)
}

func respondAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate, choices []*discordgo.ApplicationCommandOptionChoice) {
	if !usable(s, i) {
		return
	}
	st := stateOf(i)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.answered {
		return
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	}); err == nil {
		st.answered = true
	}
}
