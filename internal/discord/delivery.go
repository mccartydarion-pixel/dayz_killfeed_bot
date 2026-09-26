package discord

import (
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Delivery error classes. Only TRANSIENT and RATE_LIMITED are retried: a
// permanent failure (the channel is gone, the bot may not post there, the
// payload is rejected) needs configuration repair, and retrying it only
// burns rate limit and hides the fault.
const (
	DeliveryTransient   = "TRANSIENT"    // 5xx, network, timeouts
	DeliveryRateLimited = "RATE_LIMITED" // 429 beyond discordgo's own handling
	DeliveryConfigFault = "CONFIG_FAULT" // 404/10003 unknown channel, 403/50001/50013 access
	DeliveryRejected    = "REJECTED"     // other 4xx: payload/request will never succeed as-is
)

const (
	discordCodeUnknownGuild = 10004
)

// deliveryAttempts bounds attempts per message (1 try + 2 retries).
// deliveryBackoffBase doubles per retry; deliveryMaxRetryAfter caps how long
// a 429 Retry-After may block the calling feed goroutine.
var (
	deliveryAttempts      = 3
	deliveryBackoffBase   = 500 * time.Millisecond
	deliveryMaxRetryAfter = 10 * time.Second
	deliverySleep         = time.Sleep
)

// ClassifyDeliveryError maps a Discord send error to a delivery class.
func ClassifyDeliveryError(err error) string {
	if err == nil {
		return ""
	}
	var restErr *discordgo.RESTError
	if !errors.As(err, &restErr) {
		return DeliveryTransient // network/transport: no response at all
	}
	if restErr.Message != nil {
		switch restErr.Message.Code {
		case discordCodeUnknownChannel, discordCodeUnknownGuild, discordCodeMissingAccess, discordCodeMissingPermission:
			return DeliveryConfigFault
		}
	}
	if restErr.Response == nil {
		return DeliveryTransient
	}
	switch status := restErr.Response.StatusCode; {
	case status == 429:
		return DeliveryRateLimited
	case status == 403 || status == 404:
		return DeliveryConfigFault
	case status >= 500:
		return DeliveryTransient
	case status >= 400:
		return DeliveryRejected
	}
	return DeliveryTransient
}

func retryAfter(err error) time.Duration {
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr.Response != nil {
		if h := restErr.Response.Header.Get("Retry-After"); h != "" {
			if secs, perr := strconv.ParseFloat(h, 64); perr == nil && secs > 0 {
				return time.Duration(secs * float64(time.Second))
			}
		}
	}
	return 0
}

// messageSender is the send surface every feed publisher already uses.
type messageSender interface {
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// deliverMessage sends one message with bounded retry (see deliver).
func deliverMessage(s messageSender, route, channelID string, data *discordgo.MessageSend) (*discordgo.Message, error) {
	var msg *discordgo.Message
	err := deliver(route, channelID, func() error {
		m, err := s.ChannelMessageSendComplex(channelID, data)
		msg = m
		return err
	})
	if err != nil {
		return nil, err
	}
	return msg, nil
}

// deliver runs one Discord call with bounded retry: transient failures back
// off exponentially, a 429 waits its Retry-After (capped), and a
// configuration fault or rejected request returns immediately. The outcome is
// recorded in the process-wide delivery ledger under route. It blocks the
// calling goroutine for at most a few seconds.
func deliver(route, channelID string, call func() error) error {
	var lastErr error
	for attempt := 1; attempt <= deliveryAttempts; attempt++ {
		err := call()
		if err == nil {
			Deliveries.recordSuccess(route, channelID)
			return nil
		}
		lastErr = err
		class := ClassifyDeliveryError(err)
		if class == DeliveryConfigFault || class == DeliveryRejected || attempt == deliveryAttempts {
			Deliveries.recordFailure(route, channelID, class, err)
			if class == DeliveryConfigFault {
				slog.Error("component=discord_delivery", "event", "config_fault", "route", route, "channel_id", channelID,
					"err", truncateErr(err), "action", "repair the channel route or bot permissions (/setup repair)")
			}
			return err
		}
		wait := deliveryBackoffBase << (attempt - 1)
		if class == DeliveryRateLimited {
			if ra := retryAfter(err); ra > 0 {
				wait = ra
			}
			if wait > deliveryMaxRetryAfter {
				Deliveries.recordFailure(route, channelID, class, err)
				return err
			}
		}
		slog.Warn("component=discord_delivery", "event", "retry", "route", route, "channel_id", channelID,
			"class", class, "attempt", attempt, "retry_in", wait.String(), "err", truncateErr(err))
		deliverySleep(wait)
	}
	return lastErr
}

func truncateErr(err error) string {
	if err == nil {
		return ""
	}
	m := err.Error()
	if len(m) > 200 {
		m = m[:200]
	}
	return m
}

// RouteDelivery is one route's delivery record.
type RouteDelivery struct {
	Route               string    `json:"route"`
	ChannelID           string    `json:"channel_id,omitempty"`
	Delivered           int64     `json:"delivered"`
	Failed              int64     `json:"failed"`
	ConsecutiveFailures int64     `json:"consecutive_failures"`
	LastSuccessAt       time.Time `json:"last_success_at,omitempty"`
	LastFailureAt       time.Time `json:"last_failure_at,omitempty"`
	LastErrorClass      string    `json:"last_error_class,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
}

// State is the route's delivery state: OK, FAILING (consecutive transient
// failures) or CONFIG_FAULT (last attempt hit a permanent configuration error).
func (r RouteDelivery) State() string {
	switch {
	case r.ConsecutiveFailures > 0 && r.LastErrorClass == DeliveryConfigFault:
		return DeliveryConfigFault
	case r.ConsecutiveFailures > 0:
		return "FAILING"
	case r.Delivered > 0:
		return "OK"
	}
	return "IDLE"
}

// DeliveryLedger records per-route Discord delivery outcomes so failures are
// visible in health output instead of only in logs.
type DeliveryLedger struct {
	mu     sync.Mutex
	routes map[string]*RouteDelivery
}

// Deliveries is the process-wide ledger (one Discord guild per process).
var Deliveries = &DeliveryLedger{}

func (l *DeliveryLedger) entry(route string) *RouteDelivery {
	if l.routes == nil {
		l.routes = map[string]*RouteDelivery{}
	}
	r, ok := l.routes[route]
	if !ok {
		r = &RouteDelivery{Route: route}
		l.routes[route] = r
	}
	return r
}

func (l *DeliveryLedger) recordSuccess(route, channelID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.entry(route)
	r.ChannelID = channelID
	r.Delivered++
	r.ConsecutiveFailures = 0
	r.LastSuccessAt = time.Now()
}

func (l *DeliveryLedger) recordFailure(route, channelID, class string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.entry(route)
	r.ChannelID = channelID
	r.Failed++
	r.ConsecutiveFailures++
	r.LastFailureAt = time.Now()
	r.LastErrorClass = class
	r.LastError = truncateErr(err)
}

// RecordFailure lets callers outside this package (e.g. role assignment in
// the link service) report a delivery failure against a route.
func (l *DeliveryLedger) RecordFailure(route string, err error) {
	l.recordFailure(route, "", ClassifyDeliveryError(err), err)
}

// RecordSuccess is RecordFailure's counterpart.
func (l *DeliveryLedger) RecordSuccess(route string) { l.recordSuccess(route, "") }

// Snapshot returns every route's record, sorted by route.
func (l *DeliveryLedger) Snapshot() []RouteDelivery {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]RouteDelivery, 0, len(l.routes))
	for _, r := range l.routes {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Route < out[j].Route })
	return out
}
