package discord

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// VoiceChannelNamer is the subset of Discord ops the counter needs.
type VoiceChannelNamer interface {
	ChannelEdit(channelID string, data *discordgo.ChannelEdit) (*discordgo.Channel, error)
}

// OnlineCounterName formats the voice channel name for a given online count.
func OnlineCounterName(count int) string {
	return fmt.Sprintf("🟢・Online Players: %d", count)
}

// VoiceChannelCounter maintains a display-only voice channel whose name reflects
// the number of players online in DayZ. It renames only when the count actually
// changes, debounces bursts, and handles Discord rate limits conservatively.
//
// The count represents DayZ players (from ADM connect/disconnect events), never
// Discord voice members. The bot never joins the voice channel.
type VoiceChannelCounter struct {
	namer     VoiceChannelNamer
	channelID string

	mu            sync.Mutex
	lastPublished int // last count reflected in the channel name
	pending       int // latest known count awaiting publish
	dirty         bool
	timer         *time.Timer
	debounce      time.Duration
	blocked       bool      // 403/permission: stop retrying until repair
	retryAfter    time.Time // Discord 429: do not retry before this
	updateErrors  int
}

// NewVoiceChannelCounter creates a counter bound to a voice channel ID.
func NewVoiceChannelCounter(namer VoiceChannelNamer, channelID string) *VoiceChannelCounter {
	return &VoiceChannelCounter{
		namer:     namer,
		channelID: channelID,
		debounce:  3 * time.Second,
	}
}

// SetChannelID updates the bound voice channel (e.g. after /setup or repair).
func (c *VoiceChannelCounter) SetChannelID(id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.channelID = id
}

// ChannelID returns the bound voice channel ID.
func (c *VoiceChannelCounter) ChannelID() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.channelID
}

// UpdateErrors returns the count of failed rename attempts.
func (c *VoiceChannelCounter) UpdateErrors() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.updateErrors
}

// PermissionBlocked reports whether a 403 stopped further renames (needs repair).
func (c *VoiceChannelCounter) PermissionBlocked() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blocked
}

// LastPublished returns the count currently shown in the channel name.
func (c *VoiceChannelCounter) LastPublished() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastPublished
}

// Publish schedules a debounced rename to the given count. If the count matches
// the last published value, nothing happens. Multiple rapid changes collapse
// into one rename.
func (c *VoiceChannelCounter) Publish(count int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if count == c.lastPublished {
		c.mu.Unlock()
		return // no change; do nothing
	}
	if c.blocked {
		c.mu.Unlock()
		return // permission blocked; wait for /setup repair
	}
	c.pending = count
	c.dirty = true
	if c.timer != nil {
		c.timer.Stop()
	}
	debounce := c.debounce
	c.timer = time.AfterFunc(debounce, func() { c.flush() })
	c.mu.Unlock()
}

// flush performs the actual rename for the latest pending count.
func (c *VoiceChannelCounter) flush() {
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return
	}
	count := c.pending
	c.dirty = false
	channelID := c.channelID
	if count == c.lastPublished || c.blocked {
		c.mu.Unlock()
		return
	}
	// Honor an active Discord 429 Retry-After window.
	if time.Now().Before(c.retryAfter) {
		wait := time.Until(c.retryAfter)
		c.dirty = true
		c.timer = time.AfterFunc(wait, func() { c.flush() })
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	if c.namer == nil || channelID == "" {
		return
	}

	name := OnlineCounterName(count)
	_, err := c.namer.ChannelEdit(channelID, &discordgo.ChannelEdit{Name: name})

	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.lastPublished = count
		slog.Debug("component=discord", "msg", "online counter renamed", "count", count)
		return
	}

	c.updateErrors++
	handleRenameError(err, c)
}

// handleRenameError classifies a rename failure: 403 blocks until repair, 429
// schedules a Retry-After, and transient errors get a single bounded retry.
func handleRenameError(err error, c *VoiceChannelCounter) {
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr.Response != nil {
		status := restErr.Response.StatusCode
		switch {
		case status == 403:
			c.blocked = true
			slog.Error("component=discord", "msg", "online counter rename forbidden; run /setup repair", "status", status)
			return
		case status == 429:
			// Honor Retry-After (seconds) from the response header if present.
			wait := 5 * time.Second
			if restErr.Response != nil {
				if h := restErr.Response.Header.Get("Retry-After"); h != "" {
					if secs, perr := strconv.ParseFloat(h, 64); perr == nil && secs > 0 {
						wait = time.Duration(secs * float64(time.Second))
					}
				}
			}
			c.retryAfter = time.Now().Add(wait)
			slog.Warn("component=discord", "msg", "online counter rate limited; honoring retry-after", "wait", wait.String())
			c.dirty = true
			c.timer = time.AfterFunc(wait, func() { c.flush() })
			return
		case status >= 500:
			// Transient server error: one bounded retry.
			slog.Warn("component=discord", "msg", "online counter transient failure; retrying once", "status", status)
			c.dirty = true
			c.timer = time.AfterFunc(5*time.Second, func() { c.flush() })
			return
		}
	}
	slog.Warn("component=discord", "msg", "online counter rename failed", "err", err.Error())
}
