package discord

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The interaction router is the one place slash commands, buttons, select
// menus, forms and autocomplete are registered and dispatched. Registering a
// handler requires saying how it is acknowledged (AckMode), and the router
// then guarantees an answer:
//
//   - a handler that has not answered within ackAfter is deferred by the
//     router in its registered mode, and its eventual reply fills that in;
//   - a handler that panics is recovered, and one that returns without
//     answering (or leaves "thinking..." unfilled) gets a short failure reply;
//   - an interaction no handler is registered for is answered instead of
//     timing out.
//
// AckSelf routes (opening a form, autocomplete) cannot be deferred at Discord,
// so their handlers must answer before doing unbounded work.

// ackAfter is how long a handler may run before the router defers for it.
// Discord's limit is 3 s from when it created the interaction, so this leaves
// two seconds for gateway delivery and the acknowledgement request.
const ackAfter = time.Second

// InteractionHandler handles one routed interaction.
type InteractionHandler func(*discordgo.Session, *discordgo.InteractionCreate)

// SubAck overrides the acknowledgement mode of one subcommand ("connect", or
// "war status" for a subcommand inside a group).
type SubAck struct {
	Path string
	Ack  AckMode
}

type routeKind string

const (
	routeCommand      routeKind = "command"
	routeAutocomplete routeKind = "autocomplete"
	routeComponent    routeKind = "component"
	routeModal        routeKind = "modal"
)

type route struct {
	kind   routeKind
	match  string
	prefix bool
	ack    AckMode
	subs   map[string]AckMode
	fn     InteractionHandler
}

// RouteInfo describes one registered route.
type RouteInfo struct {
	Kind        string
	Match       string
	Prefix      bool
	Ack         AckMode
	Subcommands map[string]AckMode
}

// InteractionRouter dispatches interactions to registered routes.
type InteractionRouter struct {
	mu       sync.RWMutex
	routes   []route
	timer    *interactionTimer
	ackAfter time.Duration
}

func newInteractionRouter(timer *interactionTimer) *InteractionRouter {
	return &InteractionRouter{timer: timer, ackAfter: ackAfter}
}

func (r *InteractionRouter) add(rt route) {
	if r == nil {
		return
	}
	if rt.fn == nil {
		panic("discord: interaction route " + rt.match + " has no handler")
	}
	if rt.match == "" {
		panic("discord: interaction route has no name")
	}
	if rt.ack < AckPrivate || rt.ack > AckSelf {
		panic("discord: interaction route " + rt.match + " has no acknowledgement mode")
	}
	for path, ack := range rt.subs {
		if ack < AckPrivate || ack > AckSelf {
			panic("discord: interaction route " + rt.match + " " + path + " has no acknowledgement mode")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.routes {
		if existing.kind == rt.kind && existing.match == rt.match && existing.prefix == rt.prefix {
			panic("discord: interaction route " + rt.match + " registered twice")
		}
	}
	r.routes = append(r.routes, rt)
}

// Command routes a slash command. ack applies to every subcommand not listed
// in subs.
func (r *InteractionRouter) Command(name string, ack AckMode, fn InteractionHandler, subs ...SubAck) {
	rt := route{kind: routeCommand, match: name, ack: ack, fn: fn}
	if len(subs) > 0 {
		rt.subs = map[string]AckMode{}
		for _, sub := range subs {
			rt.subs[sub.Path] = sub.Ack
		}
	}
	r.add(rt)
}

// Autocomplete routes a command's option suggestions. Discord cannot defer
// them, so the handler must answer within the window itself.
func (r *InteractionRouter) Autocomplete(name string, fn InteractionHandler) {
	r.add(route{kind: routeAutocomplete, match: name, ack: AckSelf, fn: fn})
}

// Component routes the button or select menu with exactly this custom ID.
func (r *InteractionRouter) Component(customID string, ack AckMode, fn InteractionHandler) {
	r.add(route{kind: routeComponent, match: customID, ack: ack, fn: fn})
}

// ComponentPrefix routes buttons and select menus whose custom ID starts with
// prefix. The longest matching prefix wins.
func (r *InteractionRouter) ComponentPrefix(prefix string, ack AckMode, fn InteractionHandler) {
	r.add(route{kind: routeComponent, match: prefix, prefix: true, ack: ack, fn: fn})
}

// Modal routes the submitted form with exactly this custom ID.
func (r *InteractionRouter) Modal(customID string, ack AckMode, fn InteractionHandler) {
	r.add(route{kind: routeModal, match: customID, ack: ack, fn: fn})
}

// ModalPrefix routes submitted forms whose custom ID starts with prefix.
func (r *InteractionRouter) ModalPrefix(prefix string, ack AckMode, fn InteractionHandler) {
	r.add(route{kind: routeModal, match: prefix, prefix: true, ack: ack, fn: fn})
}

// Routes lists the registered routes, sorted by kind and match.
func (r *InteractionRouter) Routes() []RouteInfo {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]RouteInfo, 0, len(r.routes))
	for _, rt := range r.routes {
		info := RouteInfo{Kind: string(rt.kind), Match: rt.match, Prefix: rt.prefix, Ack: rt.ack}
		if len(rt.subs) > 0 {
			info.Subcommands = map[string]AckMode{}
			for path, ack := range rt.subs {
				info.Subcommands[path] = ack
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Kind != out[b].Kind {
			return out[a].Kind < out[b].Kind
		}
		return out[a].Match < out[b].Match
	})
	return out
}

// subcommandPath is "sub" or "group sub" for a command interaction.
func subcommandPath(options []*discordgo.ApplicationCommandInteractionDataOption) string {
	var parts []string
	for len(options) > 0 {
		opt := options[0]
		if opt.Type != discordgo.ApplicationCommandOptionSubCommand && opt.Type != discordgo.ApplicationCommandOptionSubCommandGroup {
			break
		}
		parts = append(parts, opt.Name)
		options = opt.Options
	}
	return strings.Join(parts, " ")
}

// find returns the route for an interaction and the mode it is acknowledged in.
func (r *InteractionRouter) find(i *discordgo.InteractionCreate) (*route, AckMode) {
	var kind routeKind
	var key, sub string
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		data := i.ApplicationCommandData()
		kind, key, sub = routeCommand, data.Name, subcommandPath(data.Options)
	case discordgo.InteractionApplicationCommandAutocomplete:
		kind, key = routeAutocomplete, i.ApplicationCommandData().Name
	case discordgo.InteractionMessageComponent:
		kind, key = routeComponent, i.MessageComponentData().CustomID
	case discordgo.InteractionModalSubmit:
		kind, key = routeModal, i.ModalSubmitData().CustomID
	default:
		return nil, 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var best *route
	for idx := range r.routes {
		rt := &r.routes[idx]
		if rt.kind != kind {
			continue
		}
		if !rt.prefix {
			if rt.match == key {
				best = rt
				break
			}
			continue
		}
		if strings.HasPrefix(key, rt.match) && (best == nil || len(rt.match) > len(best.match)) {
			best = rt
		}
	}
	if best == nil {
		return nil, 0
	}
	ack := best.ack
	if override, ok := best.subs[sub]; ok && sub != "" {
		ack = override
	}
	return best, ack
}

// Dispatch handles one interaction from the gateway.
func (r *InteractionRouter) Dispatch(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if r == nil || i == nil || i.Interaction == nil {
		return
	}
	r.timer.track(i)
	started := time.Now()
	defer func() { r.timer.finish(i, time.Since(started), isAnswered(i)) }()

	rt, ack := r.find(i)
	if rt == nil {
		answerUnrouted(s, i)
		return
	}
	var watchdog *time.Timer
	if ack != AckSelf {
		watchdog = time.AfterFunc(r.ackAfter, func() { deferAs(s, i, ack) })
	}
	runHandler(rt.fn, s, i)
	if watchdog != nil {
		watchdog.Stop()
	}
	answerIfSilent(s, i)
}

// runHandler runs a handler and recovers a panic, so one bad interaction
// cannot take the process down. Only the interaction's name is logged.
func runHandler(fn InteractionHandler, s *discordgo.Session, i *discordgo.InteractionCreate) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("component=discord", "msg", "interaction handler panicked", "interaction", interactionLabel(i),
				"panic_type", fmt.Sprintf("%T", rec), "stack", string(debug.Stack()))
		}
	}()
	fn(s, i)
}

// answerIfSilent answers an interaction whose handler ended without answering
// or without filling in its "thinking..." reply.
func answerIfSilent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !usable(s, i) {
		return
	}
	st := stateOf(i)
	st.mu.Lock()
	silent := !st.answered || (st.deferred != 0 && !st.filled)
	st.mu.Unlock()
	if !silent {
		return
	}
	if i.Type == discordgo.InteractionApplicationCommandAutocomplete {
		respondAutocomplete(s, i, nil)
		return
	}
	slog.Warn("component=discord", "msg", "interaction handler did not answer", "interaction", interactionLabel(i))
	respondPrivate(s, i, &discordgo.InteractionResponseData{Content: interactionFailureText})
}

// Texts for an interaction no handler is registered for (a feature that is
// switched off, or a button left on an old message).
const (
	unroutedCommandText   = "This command is unavailable right now."
	unroutedComponentText = "This button is no longer valid."
)

func answerUnrouted(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommandAutocomplete:
		respondAutocomplete(s, i, nil)
	case discordgo.InteractionApplicationCommand:
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: unroutedCommandText})
	case discordgo.InteractionMessageComponent, discordgo.InteractionModalSubmit:
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: unroutedComponentText})
	}
}
