// Package embedtemplates holds the durable, tenant-scoped custom embed templates of
// an installation: the typed template model, the server-side validation and
// normalization, and the small service the HTTP layer calls.
//
// Phase 2 is STORAGE AND API ONLY. Nothing here is read by a Discord publisher:
// custom template persistence is live, custom template runtime rendering is not
// enabled, and the runtime output is unchanged whether or not a template exists.
//
// The model mirrors the website's Phase 1 designer (lib/saas/embedTypes.ts). A
// template is a fixed, typed structure - never an arbitrary JSON blob - and its
// text is plain text with simple `{{variable}}` placeholders drawn from a fixed,
// per-route allowlist. There is no expression language: no functions, no
// conditionals, no evaluation of any kind.
package embedtemplates

import "time"

// SchemaVersion is stored inside config_json so the format can evolve.
const SchemaVersion = 1

// Discord-safe limits (https://discord.com/developers/docs/resources/message#embed-object-embed-limits).
const (
	MaxTitle       = 256
	MaxDescription = 4096
	MaxFields      = 25
	MaxFieldLabel  = 256
	MaxFieldValue  = 1024
	MaxFooterText  = 2048
	MaxAuthorName  = 256
	MaxURLLength   = 2048
	MaxFieldKey    = 64
	// MaxTotalText is Discord's cap on the combined text of one embed. It is
	// enforced on the enabled title, description, field labels/values, footer text
	// and author name AS WRITTEN (a placeholder counts as its literal length); the
	// runtime must additionally truncate rendered output when it is enabled.
	MaxTotalText = 6000
)

// Text is a title or description section.
type Text struct {
	Enabled  bool   `json:"enabled"`
	Template string `json:"template"`
}

// Author is the optional author section.
type Author struct {
	Enabled bool   `json:"enabled"`
	Name    string `json:"name"`
	IconURL string `json:"iconUrl"`
}

// Media is an optional thumbnail or image section.
type Media struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
}

// Footer is the optional footer section.
type Footer struct {
	Enabled bool   `json:"enabled"`
	Text    string `json:"text"`
	IconURL string `json:"iconUrl"`
}

// Field is one embed field; Order decides its position (normalized to 0..n-1).
type Field struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Enabled  bool   `json:"enabled"`
	Template string `json:"template"`
	Inline   bool   `json:"inline"`
	Order    int    `json:"order"`
}

// Config is one route's template (the website's EmbedTemplate). Sections that are
// left out decode to their zero value: disabled and empty.
type Config struct {
	Version     int     `json:"version"`
	RouteKey    string  `json:"routeKey"`
	Enabled     bool    `json:"enabled"`
	Color       string  `json:"color"`
	Title       Text    `json:"title"`
	Description Text    `json:"description"`
	Author      Author  `json:"author"`
	Thumbnail   Media   `json:"thumbnail"`
	Image       Media   `json:"image"`
	Footer      Footer  `json:"footer"`
	Timestamp   bool    `json:"timestamp"`
	Fields      []Field `json:"fields"`
}

// Stored is a persisted template with its bookkeeping.
type Stored struct {
	ID             int64
	InstallationID int64
	Config         Config
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// routeVariables is the approved variable set per route: the ONLY placeholders a
// template for that route may contain. The website's Phase 1 sets are the base and
// are only ever WIDENED (a stored template stays valid): the additions are values the
// runtime events actually carry (killfeed.Event.Ammo/KillerStreak, hit encounters,
// economy.Event.Type, bounties.Event, connection notices). A variable a given event
// does not carry is simply absent at render time (Phase 4: EMBED_RUNTIME.md).
// Aliases kept for the website: HITFEED `killer` = the attacker, BOUNTY_TRACKING
// `killer` = the hunter and `victim` = the target.
// Routes without an event vocabulary (panels, reserved routes) only get the
// generic pair. A test keeps the key set identical to the channel-route blueprint.
var routeVariables = map[string][]string{
	"KILLFEED":           {"killer", "victim", "weapon", "distance", "ammo", "streak", "special_kill", "bounty_amount", "server_name", "timestamp"},
	"PVE_FEED":           {"victim", "cause", "server_name", "timestamp"},
	"HITFEED":            {"killer", "attacker", "victim", "weapon", "ammo", "distance", "hit_zone", "damage", "hits", "server_name"},
	"BOUNTY":             {"victim", "server_name", "timestamp"},
	"BOUNTY_TRACKING":    {"killer", "victim", "target", "hunter", "amount", "total", "count", "weapon", "distance", "status", "server_name"},
	"ECONOMY":            {"player", "amount", "balance", "transaction_type", "server_name"},
	"CASINO":             {"player", "amount", "result"},
	"SHOP":               {"player", "item", "amount", "balance"},
	"CONNECTIONS":        {"player", "event", "event_type", "session", "server_name", "timestamp"},
	"BUILD_FEED":         {"player", "structure", "server_name"},
	"ADMIN_ALERTS":       {"event", "player", "server_name", "timestamp"},
	"ADMIN_LOGS":         {"event", "player", "server_name", "timestamp"},
	"HEATMAPS":           {"server_name", "timestamp"},
	"LINK_GAMERTAG":      {"server_name", "timestamp"},
	"STATS_LEADERBOARDS": {"server_name", "timestamp"},
	"AUTO_LEADERBOARD":   {"server_name", "timestamp"},
}

// RouteKeys returns every route a template may be stored for.
func RouteKeys() []string {
	out := make([]string, 0, len(routeVariables))
	for k := range routeVariables {
		out = append(out, k)
	}
	return out
}

// ValidRoute reports whether key is a route templates can be stored for.
func ValidRoute(key string) bool {
	_, ok := routeVariables[key]
	return ok
}

// Variables returns a copy of the approved variables for a route (nil if unknown).
func Variables(routeKey string) []string {
	v, ok := routeVariables[routeKey]
	if !ok {
		return nil
	}
	return append([]string(nil), v...)
}

// AllVariables returns a copy of the approved variables of every route.
func AllVariables() map[string][]string {
	out := make(map[string][]string, len(routeVariables))
	for k, v := range routeVariables {
		out[k] = append([]string(nil), v...)
	}
	return out
}
