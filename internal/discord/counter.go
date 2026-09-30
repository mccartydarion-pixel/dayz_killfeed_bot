package discord

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
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

// VoiceChannelRenamer is an optional fast-fail rename: it returns a
// *discordgo.RateLimitError on 429 instead of sleeping inside the REST client.
// A channel rename has a hidden per-channel limit (2 per 10 minutes) whose
// Retry-After can be minutes long; with discordgo's default retry the caller
// would block for all of it. *SessionAPI implements this.
type VoiceChannelRenamer interface {
	ChannelRename(channelID, name string) (*discordgo.Channel, error)
}

// CounterReading is one value the counter can display. Known=false means the
// current player count could not be established from an authoritative source;
// it is shown as "?" rather than as a manufactured number.
type CounterReading struct {
	Count int
	Slots int // server capacity; 0 when unknown
	Known bool
}

// Name is the voice channel name for this reading.
func (r CounterReading) Name() string {
	if !r.Known {
		return OnlineCounterUnknownName(r.Slots)
	}
	return OnlineCounterName(r.Count, r.Slots)
}

// OnlineCounterName formats the voice channel name for a known online count,
// e.g. "🟢・Online: 2/18" (or "🟢・Online: 2" when capacity is unknown).
func OnlineCounterName(count, slots int) string {
	if slots > 0 {
		return fmt.Sprintf("🟢・Online: %d/%d", count, slots)
	}
	return fmt.Sprintf("🟢・Online: %d", count)
}

// OnlineCounterUnknownName is the channel name while the count is unknown.
func OnlineCounterUnknownName(slots int) string {
	if slots > 0 {
		return fmt.Sprintf("⚪・Online: ?/%d", slots)
	}
	return "⚪・Online: ?"
}

// onlineCounterNameRe matches the current "Online: N/M" format and the legacy
// "Online Players: N" format, so a channel named by an older build is still
// read correctly.
var onlineCounterNameRe = regexp.MustCompile(`Online(?: Players)?:\s*(\d+|\?)(?:\s*/\s*\d+)?\s*$`)

// IsOnlineCounterName reports whether name is a counter channel name in any
// format this or an older build writes (known, unknown or legacy).
func IsOnlineCounterName(name string) bool {
	return onlineCounterNameRe.MatchString(name)
}

// ParseOnlineCounterName extracts the count shown in a counter channel name.
// known is false for an unknown ("?") or unrecognised name.
func ParseOnlineCounterName(name string) (count int, known bool) {
	m := onlineCounterNameRe.FindStringSubmatch(name)
	if m == nil || m[1] == "?" {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// DefaultCounterMinRenameInterval spaces successful renames of the counter
// channel. Discord allows 2 renames per channel per 10 minutes; one every 5
// minutes never trips that limit, so a steady stream of joins/leaves is
// coalesced into the latest value instead of queueing 429s.
const DefaultCounterMinRenameInterval = 5 * time.Minute

// VoiceChannelCounter maintains a display-only voice channel whose name reflects
// the number of players online in DayZ. It renames only when the displayed
// name actually changes, coalesces bursts into the latest value, spaces
// renames to stay inside Discord's per-channel rename limit, and never blocks
// its caller on a rate limit.
//
// The count represents DayZ players, never Discord voice members. The bot
// never joins the voice channel.
type VoiceChannelCounter struct {
	namer     VoiceChannelNamer
	channelID string

	// editMu serialises the decide-edit-record sequence so two renames are
	// never in flight at once. It is taken before mu and never while holding it.
	editMu sync.Mutex

	mu                sync.Mutex
	lastName          string // name known to be on the channel; "" until read or set
	lastPublished     int    // last KNOWN count reflected in the channel name
	pending           CounterReading
	dirty             bool
	timer             *time.Timer
	debounce          time.Duration
	minInterval       time.Duration
	lastEditAt        time.Time // last successful rename (rate-limit spacing)
	blocked           bool      // permanent configuration fault: stop until a different channel is bound
	retryAfter        time.Time // Discord 429: do not retry before this
	updateErrors      int
	lastPublishedAt   time.Time
	lastPublishResult string
	onPublish         func(count int, result string)

	// guildID, when set, is the only guild whose channel this counter may
	// rename (ownership validation on Reconcile).
	guildID string
	// faultClass/faultChannelID/faultAt record a permanent configuration
	// fault for the channel it happened on. It is cleared only by binding a
	// DIFFERENT channel (/setup repair, a route change), never by retrying.
	faultClass     string
	faultChannelID string
	faultAt        time.Time
	// transientAttempts bounds 5xx/network retries for one pending reading.
	transientAttempts int
	retryBackoff      time.Duration
	lastError         string
	lastAttemptAt     time.Time
}

// Counter fault classes: configuration problems that retrying cannot fix.
const (
	CounterFaultUnknownChannel     = "UNKNOWN_CHANNEL"     // 404 / 10003: deleted or never existed
	CounterFaultMissingPermissions = "MISSING_PERMISSIONS" // 403 / 50001 / 50013
	CounterFaultNotOwned           = "CHANNEL_NOT_OWNED"   // channel belongs to another guild
	CounterFaultWrongType          = "WRONG_CHANNEL_TYPE"  // not a voice channel
)

// maxCounterTransientRetries bounds 5xx/network retries for one pending
// reading; the next evaluation that still wants a rename starts a new budget.
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
	LastName       string    `json:"last_name,omitempty"`
	LastPublished  int       `json:"last_published"`
	LastSuccessAt  time.Time `json:"last_success_at,omitempty"`
	LastAttemptAt  time.Time `json:"last_attempt_at,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	FailedAttempts int       `json:"failed_attempts"`
}

// NewVoiceChannelCounter creates a counter bound to a voice channel ID.
func NewVoiceChannelCounter(namer VoiceChannelNamer, channelID string) *VoiceChannelCounter {
	return &VoiceChannelCounter{
		namer:       namer,
		channelID:   channelID,
		debounce:    3 * time.Second,
		minInterval: DefaultCounterMinRenameInterval,
	}
}

// SetMinRenameInterval overrides the rename spacing (tests, or an operator
// who knows the channel is renamed by nothing else).
func (c *VoiceChannelCounter) SetMinRenameInterval(d time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.minInterval = d
}

// SetDebounce overrides how long a changed reading waits to coalesce a burst.
func (c *VoiceChannelCounter) SetDebounce(d time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.debounce = d
}

// SetGuildID restricts the counter to channels of one Discord guild.
func (c *VoiceChannelCounter) SetGuildID(id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.guildID = id
}

// SetChannelID updates the bound voice channel (e.g. after /setup or repair).
// A different channel has an unknown name, its own rate-limit budget, and
// clears any fault recorded against the old one (that is the reconciliation a
// fault waits for). Re-binding the SAME ID is a no-op: a fault on it stays.
func (c *VoiceChannelCounter) SetChannelID(id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.channelID == id {
		return
	}
	if c.blocked {
		slog.Info("component=voice_counter", "event", "fault_cleared_by_rebind", "previous_channel_id", c.channelID, "channel_id", id, "fault_class", c.faultClass)
	}
	c.lastName = ""
	c.lastEditAt = time.Time{}
	c.retryAfter = time.Time{}
	c.blocked = false
	c.faultClass, c.faultChannelID, c.faultAt = "", "", time.Time{}
	c.transientAttempts = 0
	c.channelID = id
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

// Health returns the counter's delivery state for diagnostics.
func (c *VoiceChannelCounter) Health() CounterHealth {
	if c == nil {
		return CounterHealth{State: "UNBOUND"}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h := CounterHealth{
		ChannelID: c.channelID, FaultClass: c.faultClass, FaultChannelID: c.faultChannelID, FaultAt: c.faultAt,
		LastName: c.lastName, LastPublished: c.lastPublished, LastSuccessAt: c.lastPublishedAt, LastAttemptAt: c.lastAttemptAt,
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

// LastPublished returns the last known count shown in the channel name.
func (c *VoiceChannelCounter) LastPublished() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastPublished
}

// LastName returns the channel name the counter last confirmed.
func (c *VoiceChannelCounter) LastName() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastName
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

// OnPublish registers a callback run after each rename attempt. count is -1
// for an unknown reading.
func (c *VoiceChannelCounter) OnPublish(fn func(int, string)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.onPublish = fn
	c.mu.Unlock()
}

// ActualCount reads the channel's current name from Discord and parses the
// count it shows.
func (c *VoiceChannelCounter) ActualCount() (int, bool, error) {
	name, ok, err := c.actualName()
	if err != nil || !ok {
		return 0, false, err
	}
	count, known := ParseOnlineCounterName(name)
	return count, known, nil
}

func (c *VoiceChannelCounter) actualName() (string, bool, error) {
	if c == nil {
		return "", false, nil
	}
	inspector, ok := c.namer.(VoiceChannelInspector)
	if !ok || c.ChannelID() == "" {
		return "", false, nil
	}
	channelID := c.ChannelID()
	channel, err := inspector.Channel(channelID)
	if err != nil {
		c.mu.Lock()
		c.lastAttemptAt = time.Now()
		c.lastError = counterErrString(err)
		if class := permanentCounterFault(err); class != "" {
			c.recordFaultLocked(class, channelID)
		}
		c.mu.Unlock()
		return "", false, err
	}
	if channel == nil {
		return "", false, nil
	}
	c.mu.Lock()
	guildID := c.guildID
	c.mu.Unlock()
	// Ownership validation: never rename a channel of another guild or a
	// non-voice channel, even if a stored ID points at one.
	if class := channelOwnershipFault(channel, guildID); class != "" {
		c.mu.Lock()
		c.recordFaultLocked(class, channelID)
		c.mu.Unlock()
		return "", false, fmt.Errorf("online counter channel %s rejected: %s", channelID, class)
	}
	return channel.Name, true, nil
}

// channelOwnershipFault returns a fault class when channel is not a voice
// channel of guildID (guildID "" skips the guild check).
func channelOwnershipFault(channel *discordgo.Channel, guildID string) string {
	if guildID != "" && channel.GuildID != "" && channel.GuildID != guildID {
		return CounterFaultNotOwned
	}
	if channel.Type != discordgo.ChannelTypeGuildVoice {
		return CounterFaultWrongType
	}
	return ""
}

// Reconcile reads the channel's actual name from Discord and renames it now
// when it differs from the desired reading and the rate-limit spacing allows;
// otherwise the rename is scheduled for the earliest allowed moment. Use it
// when the channel may have been renamed outside this counter (startup, a
// new route); steady-state updates go through Publish.
func (c *VoiceChannelCounter) Reconcile(desired CounterReading) error {
	if c == nil {
		return nil
	}
	if c.Faulted() {
		return nil // a faulted binding waits for a different channel, never retries
	}
	actual, ok, err := c.actualName()
	if err != nil {
		return err
	}
	name := desired.Name()
	c.mu.Lock()
	if ok {
		c.lastName = actual
	}
	if ok && actual == name {
		if desired.Known {
			c.lastPublished = desired.Count
		}
		c.dirty = false
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	slog.Info("component=voice_counter", "event", "reconcile", "desired", name, "actual", actual)
	c.mu.Lock()
	c.pending, c.dirty = desired, true
	wait := c.waitLocked(time.Now())
	c.mu.Unlock()
	if wait > 0 {
		c.schedule(wait)
		return nil
	}
	return c.flushNow()
}

// Publish schedules a coalesced rename to the given reading. Nothing happens
// when the channel already shows it. Rapid changes collapse into one rename of
// the latest value, no sooner than the rate-limit spacing allows.
func (c *VoiceChannelCounter) Publish(r CounterReading) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.blocked {
		c.mu.Unlock()
		return // permission blocked; wait for /setup repair
	}
	if r.Name() == c.lastName {
		c.dirty = false
		if r.Known {
			c.lastPublished = r.Count
		}
		c.mu.Unlock()
		return // no change; do nothing
	}
	c.pending, c.dirty = r, true
	wait := c.waitLocked(time.Now())
	if wait < c.debounce {
		wait = c.debounce
	}
	c.mu.Unlock()
	c.schedule(wait)
}

// waitLocked is how long the next rename must wait for the rename spacing and
// any active Retry-After window. Caller holds mu.
func (c *VoiceChannelCounter) waitLocked(now time.Time) time.Duration {
	var wait time.Duration
	if !c.lastEditAt.IsZero() && c.minInterval > 0 {
		if d := c.lastEditAt.Add(c.minInterval).Sub(now); d > wait {
			wait = d
		}
	}
	if d := c.retryAfter.Sub(now); d > wait {
		wait = d
	}
	return wait
}

func (c *VoiceChannelCounter) schedule(wait time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		c.timer.Stop()
	}
	c.timer = time.AfterFunc(wait, func() { _ = c.flushNow() })
}

// flush performs the rename for the latest pending reading, if still due.
func (c *VoiceChannelCounter) flush() { _ = c.flushNow() }

func (c *VoiceChannelCounter) flushNow() error {
	c.editMu.Lock()
	defer c.editMu.Unlock()

	c.mu.Lock()
	if !c.dirty || c.blocked {
		c.mu.Unlock()
		return nil
	}
	reading := c.pending
	name := reading.Name()
	channelID := c.channelID
	if name == c.lastName {
		c.dirty = false
		c.mu.Unlock()
		return nil
	}
	if wait := c.waitLocked(time.Now()); wait > 0 {
		c.mu.Unlock()
		c.schedule(wait)
		return nil
	}
	c.dirty = false
	c.mu.Unlock()

	if c.namer == nil || channelID == "" {
		return nil
	}
	c.mu.Lock()
	c.lastAttemptAt = time.Now()
	c.mu.Unlock()
	var err error
	if renamer, ok := c.namer.(VoiceChannelRenamer); ok {
		_, err = renamer.ChannelRename(channelID, name)
	} else {
		_, err = c.namer.ChannelEdit(channelID, &discordgo.ChannelEdit{Name: name})
	}

	count := reading.Count
	if !reading.Known {
		count = -1
	}
	c.mu.Lock()
	callback := c.onPublish
	if err == nil {
		c.lastName = name
		c.transientAttempts = 0
		c.lastError = ""
		if reading.Known {
			c.lastPublished = reading.Count
		}
		c.lastEditAt = time.Now()
		c.lastPublishedAt = c.lastEditAt
		c.lastPublishResult = "SUCCESS"
		c.mu.Unlock()
		if callback != nil {
			go callback(count, "SUCCESS")
		}
		slog.Info("component=voice_counter", "event", "channel_edit_success", "name", name, "known", reading.Known)
		return nil
	}
	c.updateErrors++
	c.lastPublishResult = "FAILURE"
	c.lastError = counterErrString(err)
	retry := handleRenameError(err, c, channelID)
	c.mu.Unlock()
	if callback != nil {
		go callback(count, "FAILURE")
	}
	if retry > 0 {
		c.schedule(retry)
	}
	return err
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
	c.faultClass, c.faultChannelID, c.faultAt = class, channelID, time.Now()
	if c.timer != nil {
		c.timer.Stop()
	}
	slog.Error("component=voice_counter", "event", "config_fault", "fault_class", class, "channel_id", channelID,
		"msg", "online counter renames stopped until the channel is repaired", "action", "run /setup repair or re-map the ONLINE_COUNTER route")
}

// handleRenameError classifies a rename failure and returns how long to wait
// before retrying (0 = no automatic retry; the next Publish retries).
// Permanent faults (403, 404, Unknown Channel, missing access) stop renames
// for this channel; 429 honours Retry-After without blocking; 5xx and network
// errors get bounded exponential retries. Caller holds mu.
func handleRenameError(err error, c *VoiceChannelCounter, channelID string) time.Duration {
	if class := permanentCounterFault(err); class != "" {
		c.recordFaultLocked(class, channelID)
		return 0
	}
	var rateErr *discordgo.RateLimitError
	if errors.As(err, &rateErr) && rateErr.RateLimit != nil && rateErr.TooManyRequests != nil {
		wait := rateErr.RetryAfter
		if wait <= 0 {
			wait = 5 * time.Second
		}
		c.retryAfter = time.Now().Add(wait)
		c.dirty = true
		slog.Warn("component=voice_counter", "event", "rate_limited", "wait", wait.String())
		return wait
	}
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr.Response != nil && restErr.Response.StatusCode == 429 {
		wait := 5 * time.Second
		if h := restErr.Response.Header.Get("Retry-After"); h != "" {
			if secs, perr := strconv.ParseFloat(h, 64); perr == nil && secs > 0 {
				wait = time.Duration(secs * float64(time.Second))
			}
		}
		c.retryAfter = time.Now().Add(wait)
		c.dirty = true
		slog.Warn("component=voice_counter", "event", "rate_limited", "wait", wait.String())
		return wait
	}
	// Transient (5xx, network, unknown): bounded exponential retry.
	c.transientAttempts++
	if c.transientAttempts > maxCounterTransientRetries {
		slog.Warn("component=voice_counter", "event", "retry_budget_exhausted", "attempts", c.transientAttempts, "err", c.lastError)
		c.transientAttempts = 0
		return 0
	}
	base := c.retryBackoff
	if base <= 0 {
		base = 5 * time.Second
	}
	wait := base << (c.transientAttempts - 1)
	c.dirty = true
	slog.Warn("component=voice_counter", "event", "transient_failure", "attempt", c.transientAttempts, "retry_in", wait.String(), "err", c.lastError)
	return wait
}

func counterErrString(err error) string {
	if err == nil {
		return ""
	}
	m := err.Error()
	if len(m) > 200 {
		m = m[:200]
	}
	return m
}
