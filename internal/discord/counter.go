package discord

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// VoiceChannelNamer is the subset of Discord ops the counter needs.
type VoiceChannelNamer interface {
	ChannelEdit(channelID string, data *discordgo.ChannelEdit) (*discordgo.Channel, error)
}

type VoiceChannelInspector interface {
	Channel(channelID string) (*discordgo.Channel, error)
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

	mu                sync.Mutex
	lastPublished     int // last count reflected in the channel name
	pending           int // latest known count awaiting publish
	dirty             bool
	timer             *time.Timer
	debounce          time.Duration
	blocked           bool      // 403/permission: stop retrying until repair
	retryAfter        time.Time // Discord 429: do not retry before this
	updateErrors      int
	lastPublishedAt   time.Time
	lastPublishResult string
	onPublish         func(count int, result string)
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

func (c *VoiceChannelCounter) LastPublishedAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastPublishedAt
}

func (c *VoiceChannelCounter) LastPublishResult() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastPublishResult
}

func (c *VoiceChannelCounter) OnPublish(fn func(int, string)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.onPublish = fn
	c.mu.Unlock()
}

func (c *VoiceChannelCounter) ActualCount() (int, bool, error) {
	if c == nil {
		return 0, false, nil
	}
	inspector, ok := c.namer.(VoiceChannelInspector)
	if !ok {
		return 0, false, nil
	}
	channel, err := inspector.Channel(c.ChannelID())
	if err != nil || channel == nil {
		return 0, false, err
	}
	marker := "Online Players:"
	idx := strings.LastIndex(channel.Name, marker)
	if idx < 0 {
		return 0, false, nil
	}
	value := strings.TrimSpace(channel.Name[idx+len(marker):])
	count, err := strconv.Atoi(value)
	if err != nil || count < 0 {
		return 0, false, err
	}
	return count, true, nil
}

// Reconcile fetches the authoritative channel name and edits only when its
// actual numeric count differs from the selected worker's desired count.
func (c *VoiceChannelCounter) Reconcile(desired int) error {
	actual, known, err := c.ActualCount()
	if err != nil {
		return err
	}
	if known && actual == desired {
		c.mu.Lock()
		c.lastPublished = actual
		c.mu.Unlock()
		return nil
	}
	slog.Info("component=voice_counter", "event", "reconcile", "desired", desired, "actual", actual)
	return c.editConfirmed(desired, actual)
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

	_, err := c.namer.ChannelEdit(channelID, &discordgo.ChannelEdit{Name: OnlineCounterName(count)})

	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.lastPublished = count
		c.lastPublishedAt = time.Now()
		c.lastPublishResult = "SUCCESS"
		callback := c.onPublish
		if callback != nil {
			go callback(count, "SUCCESS")
		}
		slog.Info("component=presence", "event", "voice_counter_publish", "count", count, "result", "success")
		slog.Debug("component=discord", "msg", "online counter renamed", "count", count)
		return
	}

	c.updateErrors++
	c.lastPublishResult = "FAILURE"
	callback := c.onPublish
	if callback != nil {
		go callback(count, "FAILURE")
	}
	handleRenameError(err, c)
}

func (c *VoiceChannelCounter) editConfirmed(count, previous int) error {
	if c == nil || c.namer == nil || c.ChannelID() == "" {
		return nil
	}
	slog.Info("component=voice_counter", "event", "channel_edit_started", "desired", count)
	_, err := c.namer.ChannelEdit(c.ChannelID(), &discordgo.ChannelEdit{Name: OnlineCounterName(count)})
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.updateErrors++
		c.lastPublishResult = "FAILURE"
		handleRenameError(err, c)
		slog.Warn("component=voice_counter", "event", "channel_edit_failed", "desired", count, "error_class", "discord_edit_failed")
		return err
	}
	c.lastPublished = count
	c.lastPublishedAt = time.Now()
	c.lastPublishResult = "SUCCESS"
	if c.onPublish != nil {
		go c.onPublish(count, "SUCCESS")
	}
	slog.Info("component=voice_counter", "event", "channel_edit_success", "previous", previous, "current", count)
	return nil
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
