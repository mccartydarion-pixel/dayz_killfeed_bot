package app

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The Embed Designer's emoji picker (docs/EMBED_DESIGNER_V2.md "Emojis"): the custom emojis
// uploaded to the bot's own application in the Discord Developer Portal. They belong to the bot,
// not to a server, so they render in every server it posts to and every installation sees the
// same list. Read-only: nothing here creates, renames or deletes an emoji.

type embedEmojiDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Animated bool   `json:"animated"`
}

type embedEmojiListResponse struct {
	Emojis []embedEmojiDTO `json:"emojis"`
}

// embedEmojiTTL: the list changes only when somebody uploads in the Developer Portal.
const embedEmojiTTL = 2 * time.Minute

var (
	embedEmojiIDRe   = regexp.MustCompile(`^\d{15,22}$`)
	embedEmojiNameRe = regexp.MustCompile(`^[A-Za-z0-9_]{2,32}$`)
)

// embedEmojiCache holds the last list read from Discord.
type embedEmojiCache struct {
	mu      sync.Mutex
	at      time.Time
	emojis  []embedEmojiDTO
	loading sync.Mutex // one Discord read at a time
}

// embedEmojis cleans a Discord emoji list: well-formed entries only, sorted by name.
func embedEmojis(raw []*discordgo.Emoji) []embedEmojiDTO {
	out := make([]embedEmojiDTO, 0, len(raw))
	for _, e := range raw {
		if e == nil || !embedEmojiIDRe.MatchString(e.ID) || !embedEmojiNameRe.MatchString(e.Name) {
			continue
		}
		out = append(out, embedEmojiDTO{ID: e.ID, Name: e.Name, Animated: e.Animated})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// get returns the cached list, reading it with load when it is older than embedEmojiTTL. A
// failed read returns the previous list when there is one.
func (c *embedEmojiCache) get(now time.Time, load func() ([]*discordgo.Emoji, error)) ([]embedEmojiDTO, error) {
	c.mu.Lock()
	if !c.at.IsZero() && now.Sub(c.at) < embedEmojiTTL {
		out := c.emojis
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	c.loading.Lock()
	defer c.loading.Unlock()
	c.mu.Lock()
	if !c.at.IsZero() && now.Sub(c.at) < embedEmojiTTL { // another request just loaded it
		out := c.emojis
		c.mu.Unlock()
		return out, nil
	}
	previous, had := c.emojis, !c.at.IsZero()
	c.mu.Unlock()

	raw, err := load()
	if err != nil {
		if had {
			return previous, nil
		}
		return nil, err
	}
	out := embedEmojis(raw)
	c.mu.Lock()
	c.emojis, c.at = out, now
	c.mu.Unlock()
	return out, nil
}

var embedEmojiList embedEmojiCache

// handleListEmbedEmojis is GET .../installations/{installationID}/embed-emojis. Anyone who may
// open the Embed Designer may read it.
func (a *App) handleListEmbedEmojis(w http.ResponseWriter, r *http.Request) {
	if _, _, _, _, ok := a.embedTemplateContext(w, r, false, false); !ok {
		return
	}
	if a.Discord == nil || a.Discord.Session() == nil {
		writeSaaSError(w, codeDiscordUnavailable, "Discord is not connected")
		return
	}
	session := a.Discord.Session()
	appID := a.Discord.BotID()
	if session.State != nil && session.State.Application != nil && session.State.Application.ID != "" {
		appID = session.State.Application.ID
	}
	if appID == "" {
		writeSaaSError(w, codeDiscordUnavailable, "Discord is not connected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	emojis, err := embedEmojiList.get(time.Now(), func() ([]*discordgo.Emoji, error) {
		return session.ApplicationEmojis(appID, discordgo.WithContext(ctx))
	})
	if err != nil {
		slog.Warn("component=saas_api", "event", "embed_emojis_failed", "err", err.Error())
		writeSaaSError(w, codeDiscordUnavailable, "could not read the bot's emojis from Discord")
		return
	}
	writeSaaSJSON(w, http.StatusOK, embedEmojiListResponse{Emojis: emojis})
}
