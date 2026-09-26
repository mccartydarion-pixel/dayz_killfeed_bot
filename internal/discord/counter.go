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

	// editMu serialises the decide-edit-record sequence in flush and
	// editConfirmed so two renames are never in flight at once. Without it a
	// timer-driven flush and a caller-driven flush interleave, losing or
	// reordering renames. It is taken before mu and never while holding it.
	editMu sync.Mutex

	mu                sync.Mutex
	lastPublished     int // last count reflected in the channel name
	pending           int // latest known count awaiting publish
	dirty             bool
	timer             *time.Timer
	debounce          time.Duration
	blocked           bool      // permanent fault (403/404): stop until the binding changes
	retryAfter        time.Time // Discord 429: do not retry before this
	updateErrors      int
	lastPublishedAt   time.Time
	lastPublishResult string
	onPublish         func(count int, result string)

	// publishedUnknown is set when the bound channel changes: the new
	// channel's name is unknown, so the next count must be written even if it
	// equals lastPublished on the old channel.
	publishedUnknown bool
	// faultClass/faultChannelID record a permanent configuration fault
	// (UNKNOWN_CHANNEL, MISSING_PERMISSIONS) for the channel it happened on.
	// It needs reconciliation (a new binding), never blind retries.
	faultClass     string
	faultChannelID string
	faultAt        time.Time
	// transientAttempts bounds 5xx/network retries for one pending count.
	transientAttempts int
	lastError         string
	lastAttemptAt     time.Time
	retryBackoff      time.Duration
}

// Counter fault classes. A fault is a configuration problem, not a transient
// delivery failure: the counter stops calling Discord for that channel until
// SetChannelID binds a different one (/setup repair or a route change).
const (
	CounterFaultUnknownChannel     = "UNKNOWN_CHANNEL"
	CounterFaultMissingPermissions = "MISSING_PERMISSIONS"
)

// maxCounterTransientRetries bounds retries for 5xx/network failures of one
// pending count; the next presence change starts a fresh budget.
const maxCounterTransientRetries = 3

// Discord JSON error codes that make a channel permanently unusable.
const (
	discordCodeUnknownChannel    = 10003
	discordCodeMissingAccess     = 50001
	discordCodeMissingPermission = 50013
)

// CounterHealth is the counter's delivery state for diagnostics.
type CounterHealth struct {
	ChannelID      string    `json:"channel_id"`
	State          string    `json:"state"` // UNBOUND | OK | PENDING | RETRYING | RATE_LIMITED | CONFIG_FAULT
	FaultClass     string    `json:"fault_class,omitempty"`
	FaultChannelID string    `json:"fault_channel_id,omitempty"`
	FaultAt        time.Time `json:"fault_at,omitempty"`
	LastPublished  int       `json:"last_published"`
	LastSuccessAt  time.Time `json:"last_success_at,omitempty"`
	LastAttemptAt  time.Time `json:"last_attempt_at,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	FailedAttempts int       `json:"failed_attempts"`
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
// Binding a different channel clears any fault recorded against the old one
// (that is the reconciliation a fault waits for) and forces the next count to
// be written, since the new channel's current name is unknown. Re-binding the
// same ID is a no-op, so a fault on it stays in place.
func (c *VoiceChannelCounter) SetChannelID(id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == c.channelID {
		return
	}
	previous := c.channelID
	c.channelID = id
	c.publishedUnknown = true
	c.transientAttempts = 0
	if c.blocked || c.faultClass != "" {
		slog.Info("component=voice_counter", "event", "fault_cleared_by_rebind", "previous_channel_id", previous, "channel_id", id, "fault_class", c.faultClass)
	}
	c.blocked = false
	c.faultClass = ""
	c.faultChannelID = ""
	c.faultAt = time.Time{}
}

// Health returns the counter's delivery state for diagnostics.
func (c *VoiceChannelCounter) Health() CounterHealth {
	if c == nil {
		return CounterHealth{State: "UNBOUND"}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h := CounterHealth{
		ChannelID: c.channelID, FaultClass: c.faultClass, FaultChannelID: c.faultChannelID, FaultAt: c.faultAt,
		LastPublished: c.lastPublished, LastSuccessAt: c.lastPublishedAt, LastAttemptAt: c.lastAttemptAt,
		LastError: c.lastError, FailedAttempts: c.updateErrors,
	}
	switch {
	case c.channelID == "":
		h.State = "UNBOUND"
	case c.blocked:
		h.State = "CONFIG_FAULT"
	case time.Now().Before(c.retryAfter):
		h.State = "RATE_LIMITED"
	case c.transientAttempts > 0:
		h.State = "RETRYING"
	case c.dirty:
		h.State = "PENDING"
	default:
		h.State = "OK"
	}
	return h
}

// Faulted reports whether a permanent configuration fault stopped renames.
func (c *VoiceChannelCounter) Faulted() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blocked
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
	if c == nil {
		return nil
	}
	if c.Faulted() {
		// A faulted binding is not retried on every presence change; it
		// waits for SetChannelID to bind a working channel.
		return nil
	}
	actual, known, err := c.ActualCount()
	if err != nil {
		c.mu.Lock()
		c.lastAttemptAt = time.Now()
		c.lastError = sanitizeCounterError(err)
		if class := permanentCounterFault(err); class != "" {
			c.recordFaultLocked(class, c.channelID)
		}
		c.mu.Unlock()
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
	if count == c.lastPublished && !c.publishedUnknown {
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
	c.editMu.Lock()
	defer c.editMu.Unlock()

	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return
	}
	count := c.pending
	c.dirty = false
	channelID := c.channelID
	if (count == c.lastPublished && !c.publishedUnknown) || c.blocked {
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
	c.lastAttemptAt = time.Now()
	if err == nil {
		c.lastPublished = count
		c.publishedUnknown = false
		c.transientAttempts = 0
		c.lastError = ""
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
	c.lastError = sanitizeCounterError(err)
	callback := c.onPublish
	if callback != nil {
		go callback(count, "FAILURE")
	}
	handleRenameError(err, c, channelID, true)
}

func (c *VoiceChannelCounter) editConfirmed(count, previous int) error {
	if c == nil || c.namer == nil || c.ChannelID() == "" {
		return nil
	}
	c.editMu.Lock()
	defer c.editMu.Unlock()
	slog.Info("component=voice_counter", "event", "channel_edit_started", "desired", count)
	channelID := c.ChannelID()
	_, err := c.namer.ChannelEdit(channelID, &discordgo.ChannelEdit{Name: OnlineCounterName(count)})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastAttemptAt = time.Now()
	if err != nil {
		c.updateErrors++
		c.lastPublishResult = "FAILURE"
		c.lastError = sanitizeCounterError(err)
		// Reconcile is re-driven by the next presence change, so it only
		// classifies here; it never schedules its own retries.
		handleRenameError(err, c, channelID, false)
		slog.Warn("component=voice_counter", "event", "channel_edit_failed", "desired", count, "error_class", "discord_edit_failed")
		return err
	}
	c.publishedUnknown = false
	c.transientAttempts = 0
	c.lastError = ""
	c.lastPublished = count
	c.lastPublishedAt = time.Now()
	c.lastPublishResult = "SUCCESS"
	if c.onPublish != nil {
		go c.onPublish(count, "SUCCESS")
	}
	slog.Info("component=voice_counter", "event", "channel_edit_success", "previous", previous, "current", count)
	return nil
}

// permanentCounterFault returns the fault class for errors that retrying can
// never fix: the channel is gone (404 / 10003) or the bot may not manage it
// (403 / 50001 / 50013). Anything else is "" (transient or unknown).
func permanentCounterFault(err error) string {
	var restErr *discordgo.RESTError
	if !errors.As(err, &restErr) {
		return ""
	}
	if restErr.Message != nil {
		switch restErr.Message.Code {
		case discordCodeUnknownChannel:
			return CounterFaultUnknownChannel
		case discordCodeMissingAccess, discordCodeMissingPermission:
			return CounterFaultMissingPermissions
		}
	}
	if restErr.Response != nil {
		switch restErr.Response.StatusCode {
		case 404:
			return CounterFaultUnknownChannel
		case 403:
			return CounterFaultMissingPermissions
		}
	}
	return ""
}

// recordFaultLocked marks a permanent configuration fault. Caller holds c.mu.
func (c *VoiceChannelCounter) recordFaultLocked(class, channelID string) {
	if c.blocked && c.faultClass == class && c.faultChannelID == channelID {
		return
	}
	c.blocked = true
	c.dirty = false
	c.transientAttempts = 0
	c.faultClass = class
	c.faultChannelID = channelID
	c.faultAt = time.Now()
	if c.timer != nil {
		c.timer.Stop()
	}
	slog.Error("component=discord", "msg", "online counter channel configuration fault; renames stopped until the channel is repaired",
		"fault_class", class, "channel_id", channelID, "action", "run /setup repair or re-map the ONLINE_COUNTER route")
}

// handleRenameError classifies a rename failure. Permanent faults (403, 404,
// Unknown Channel) stop renames for that channel until it is re-bound; 429
// schedules a Retry-After; 5xx and network errors get bounded exponential
// retries when schedule is set (the debounced publish path). Caller holds c.mu.
func handleRenameError(err error, c *VoiceChannelCounter, channelID string, schedule bool) {
	if class := permanentCounterFault(err); class != "" {
		c.recordFaultLocked(class, channelID)
		return
	}
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr.Response != nil && restErr.Response.StatusCode == 429 {
		// Honor Retry-After (seconds) from the response header if present.
		wait := 5 * time.Second
		if h := restErr.Response.Header.Get("Retry-After"); h != "" {
			if secs, perr := strconv.ParseFloat(h, 64); perr == nil && secs > 0 {
				wait = time.Duration(secs * float64(time.Second))
			}
		}
		c.retryAfter = time.Now().Add(wait)
		slog.Warn("component=discord", "msg", "online counter rate limited; honoring retry-after", "wait", wait.String())
		if schedule {
			c.dirty = true
			c.timer = time.AfterFunc(wait, func() { c.flush() })
		}
		return
	}
	// Transient (5xx, network, unknown): bounded exponential retry.
	c.transientAttempts++
	if !schedule {
		slog.Warn("component=discord", "msg", "online counter rename failed", "err", c.lastError, "attempt", c.transientAttempts)
		return
	}
	if c.transientAttempts > maxCounterTransientRetries {
		slog.Warn("component=discord", "msg", "online counter rename failed; retry budget exhausted until the next presence change",
			"err", c.lastError, "attempts", c.transientAttempts)
		c.transientAttempts = 0
		return
	}
	base := c.retryBackoff
	if base <= 0 {
		base = 5 * time.Second
	}
	wait := base << (c.transientAttempts - 1)
	slog.Warn("component=discord", "msg", "online counter transient failure; retrying", "err", c.lastError, "attempt", c.transientAttempts, "retry_in", wait.String())
	c.dirty = true
	c.timer = time.AfterFunc(wait, func() { c.flush() })
}

// sanitizeCounterError bounds an error for diagnostics output.
func sanitizeCounterError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
