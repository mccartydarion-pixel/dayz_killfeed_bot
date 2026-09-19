//go:build integration

package app

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/embedrender"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// runtimeWorld: the economy world (one org, one guild, TWO servers with their own
// installations and ECONOMY routes) with a REAL renderer over the real repository, the
// real template PUT/DELETE handlers, and the real feed.
type runtimeWorld struct {
	*economyWorld
	renderer *embedrender.Renderer
	admin    *embedWorld // only used for its HTTP call helpers
	rich     *richSender
}

// richSender records the whole delivered card (title, description and fields), unlike
// the older test sender which keeps only the description.
type richSender struct {
	mu    sync.Mutex
	cards map[string][]string
}

func (s *richSender) ChannelMessageSendComplex(ch string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range data.Embeds {
		text := strings.TrimSpace(e.Title + "\n" + e.Description)
		for _, f := range e.Fields {
			text += "\n[" + f.Name + ": " + f.Value + "]"
		}
		s.cards[ch] = append(s.cards[ch], text)
	}
	return &discordgo.Message{ID: "m", ChannelID: ch}, nil
}

func (s *richSender) in(ch string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cards[ch]...)
}

func serverName(id int64) string { return "Server-" + strconv.FormatInt(id, 10) }

func newRuntimeWorld(t *testing.T, ttl time.Duration) *runtimeWorld {
	t.Helper()
	ew := newEconomyWorld(t, "eco-A", "eco-B")
	repo := repository.NewEmbedTemplateRepository(ew.a.DB.Pool)
	r := embedrender.New(embedrender.Options{Source: repo, Enabled: true, TTL: ttl})
	ew.a.EmbedTemplates = embedtemplates.NewService(repo)
	ew.a.EmbedRenderer = r
	rich := &richSender{cards: map[string][]string{}}
	feed := discord.NewEconomyFeed(rich, ew.a.ChannelRoutes, ew.servers) // the real feed, delivering to the recording sender
	feed.SetCustomizer(r, serverName)
	ew.svc.SetNotifier(feed)
	ew.feed = feed
	return &runtimeWorld{economyWorld: ew, renderer: r, admin: &embedWorld{t: t, a: ew.a}, rich: rich}
}

func ecoTemplate(title string) map[string]any {
	return map[string]any{
		"enabled": true, "color": "#4c9a72",
		"title":       map[string]any{"enabled": true, "template": title},
		"description": map[string]any{"enabled": true, "template": "{{player}} {{transaction_type}}: {{amount}} on {{server_name}}"},
		"footer":      map[string]any{"enabled": true, "text": "Economy", "iconUrl": ""},
		"timestamp":   true,
		"fields": []map[string]any{
			{"key": "balance", "label": "Balance", "enabled": true, "template": "{{balance}}", "inline": true, "order": 0},
			{"key": "amount", "label": "Amount", "enabled": true, "template": "{{amount}}", "inline": true, "order": 1},
		},
	}
}

func (w *runtimeWorld) putEco(inst int64, title string) {
	w.t.Helper()
	if rr := w.admin.put(w.fixture.OrgID, inst, "ECONOMY", w.fixture.OwnerDiscordID, ecoTemplate(title)); rr.Code != http.StatusOK {
		w.t.Fatalf("save template: %d %s", rr.Code, rr.Body.String())
	}
}

func (w *runtimeWorld) delEco(inst int64) {
	w.t.Helper()
	if rr := w.admin.del(w.fixture.OrgID, inst, "ECONOMY", w.fixture.OwnerDiscordID); rr.Code != http.StatusOK {
		w.t.Fatalf("reset template: %d %s", rr.Code, rr.Body.String())
	}
}

// cardsIn flushes the feed and returns the embeds delivered to a channel since last time.
func (w *runtimeWorld) publish(channel string) []string {
	w.t.Helper()
	before := len(w.rich.in(channel))
	w.feed.Flush()
	return w.rich.in(channel)[before:]
}

func TestEmbedRuntimeStoreRenderResetAndChangeAgainstRealPostgres(t *testing.T) {
	w := newRuntimeWorld(t, time.Hour) // the TTL cannot explain immediacy: only invalidation can
	instA := w.fixture.InstallationID

	// 1. No custom template: the existing Champion card, unchanged.
	if _, err := w.adminCredit(w.hunter, 50000, "bonus"); err != nil {
		t.Fatal(err)
	}
	got := w.publish("eco-A")
	if len(got) != 1 || got[0] != "➕ **ADMIN CREDIT**\nHunter received 50,000 pts" {
		t.Fatalf("no template = the default card: %q", got)
	}

	// 2. Save through the real API: the very next event renders the custom card. (The
	// cached "no template" answer was invalidated by the PUT, not by the TTL.)
	w.putEco(instA, "{{player}} got paid")
	if _, err := w.adminCredit(w.hunter, 70000, "bonus 2"); err != nil {
		t.Fatal(err)
	}
	custom := w.publish("eco-A")
	if len(custom) != 1 || !strings.Contains(custom[0], "Hunter got paid") {
		t.Fatalf("the saved template must render: %q", custom)
	}
	if !strings.Contains(custom[0], "Hunter Admin credit: 70,000 on Server-") {
		t.Fatalf("variables: player, transaction_type, amount and server_name: %q", custom)
	}
	// The optional balance is absent for an admin credit, so that field is omitted and the
	// Amount field stays - never "Balance: N/A".
	if strings.Contains(custom[0], "[Balance:") || !strings.Contains(custom[0], "[Amount: 70,000]") {
		t.Fatalf("an absent optional value omits its field: %q", custom)
	}
	// A guild-wide credit reaches both servers; only server A has a template.
	if s := w.renderer.Stats(); s.CustomRender != 1 || s.FallbackRender != 0 {
		t.Fatalf("stats: %+v", s)
	}

	// 3. Change it: again immediate.
	w.putEco(instA, "{{player}} got paid AGAIN")
	if _, err := w.adminCredit(w.hunter, 1, ""); err != nil {
		t.Fatal(err)
	}
	if c := w.publish("eco-A"); len(c) != 1 || !strings.Contains(c[0], "got paid AGAIN") {
		t.Fatalf("a changed template must render immediately: %q", c)
	}

	// 4. Reset (DELETE): the Champion default is back on the next event.
	w.delEco(instA)
	if _, err := w.adminCredit(w.hunter, 5, ""); err != nil {
		t.Fatal(err)
	}
	if c := w.publish("eco-A"); len(c) != 1 || c[0] != "➕ **ADMIN CREDIT**\nHunter received 5 pts" {
		t.Fatalf("after DELETE the default card returns: %q", c)
	}
}

// Same guild, two servers: a custom template on server A must not touch server B.
func TestEmbedRuntimeMultiServerIsolation(t *testing.T) {
	w := newRuntimeWorld(t, time.Hour)
	w.putEco(w.fixture.InstallationID, "SERVER-A-STYLE")

	// A guild-wide admin credit fans out to both servers' ECONOMY routes.
	if _, err := w.adminCredit(w.hunter, 900, "x"); err != nil {
		t.Fatal(err)
	}
	beforeA, beforeB := len(w.rich.in("eco-A")), len(w.rich.in("eco-B"))
	w.feed.Flush()
	a, b := w.rich.in("eco-A")[beforeA:], w.rich.in("eco-B")[beforeB:]
	if len(a) != 1 || !strings.Contains(a[0], "SERVER-A-STYLE") {
		t.Fatalf("server A uses its template: %q", a)
	}
	if len(b) != 1 || b[0] != "➕ **ADMIN CREDIT**\nHunter received 900 pts" {
		t.Fatalf("server B (same guild) must keep the default: %q", b)
	}
}

// Another organization's template never applies to this organization's servers, and
// this organization cannot write it.
func TestEmbedRuntimeCrossOrganizationSafety(t *testing.T) {
	w := newRuntimeWorld(t, time.Hour)
	other := buildInstallationFixture(t, w.a, w.verifier)
	connectNitrado(t, w.a, other.OrgID, other.OwnerDiscordID)
	otherServer := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, w.a, other.OrgID, other.InstallationID, 111111, other.OwnerDiscordID)).Server.ID
	_ = otherServer
	if rr := w.admin.put(other.OrgID, other.InstallationID, "ECONOMY", other.OwnerDiscordID, ecoTemplate("OTHER-ORG-STYLE")); rr.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", rr.Code, rr.Body.String())
	}
	// The other organization's owner cannot reach OUR installation.
	if rr := w.admin.put(w.fixture.OrgID, w.fixture.InstallationID, "ECONOMY", other.OwnerDiscordID, ecoTemplate("HIJACK")); rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
	if rr := w.admin.put(other.OrgID, w.fixture.InstallationID, "ECONOMY", other.OwnerDiscordID, ecoTemplate("HIJACK")); rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
	if _, err := w.adminCredit(w.hunter, 10, ""); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []string{"eco-A", "eco-B"} {
		for _, card := range w.publish(ch) {
			if strings.Contains(card, "OTHER-ORG-STYLE") || strings.Contains(card, "HIJACK") {
				t.Fatalf("a foreign template leaked into %s: %q", ch, card)
			}
		}
	}
}

// An out-of-process change (a direct database edit) is picked up within the TTL.
func TestEmbedRuntimeTTLPicksUpOutOfProcessChanges(t *testing.T) {
	w := newRuntimeWorld(t, 150*time.Millisecond)
	if _, err := w.adminCredit(w.hunter, 1, ""); err != nil {
		t.Fatal(err)
	}
	if c := w.publish("eco-A"); len(c) != 1 || strings.Contains(c[0], "OUT-OF-PROCESS") {
		t.Fatalf("default first: %q", c)
	}
	cfg, err := embedtemplates.Validate(embedtemplates.Config{Enabled: true, Color: "#123456", Title: embedtemplates.Text{Enabled: true, Template: "OUT-OF-PROCESS {{player}}"}}, "ECONOMY")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.NewEmbedTemplateRepository(w.a.DB.Pool).Upsert(context.Background(), w.fixture.OrgID, w.fixture.InstallationID, cfg); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond) // past the TTL, and no Invalidate call
	if _, err := w.adminCredit(w.hunter, 2, ""); err != nil {
		t.Fatal(err)
	}
	if c := w.publish("eco-A"); len(c) != 1 || !strings.Contains(c[0], "OUT-OF-PROCESS Hunter") {
		t.Fatalf("the new template must appear within the TTL: %q", c)
	}
}

// The template lookup failing never costs the event its card, or the balance its commit.
func TestEmbedRuntimeDatabaseFailureFallsBackToTheDefault(t *testing.T) {
	w := newRuntimeWorld(t, time.Hour)
	w.putEco(w.fixture.InstallationID, "CUSTOM-STYLE")
	w.renderer.InvalidateAll()

	// A renderer whose own connection pool is closed (its lookups fail); the feed and
	// the economy service keep the healthy pool.
	brokenPool, err := pgxpool.New(context.Background(), w.a.DB.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	brokenPool.Close()
	broken := embedrender.New(embedrender.Options{Source: repository.NewEmbedTemplateRepository(brokenPool), Enabled: true})
	w.feed.SetCustomizer(broken, serverName)

	before := w.balance(w.hunter)
	if _, err := w.adminCredit(w.hunter, 333, "x"); err != nil {
		t.Fatal(err)
	}
	c := w.publish("eco-A")
	if len(c) != 1 || c[0] != "➕ **ADMIN CREDIT**\nHunter received 333 pts" {
		t.Fatalf("a failing template lookup must publish the Champion default: %q", c)
	}
	if w.balance(w.hunter) != before+333 {
		t.Fatal("the transaction is committed regardless of the template lookup")
	}
	if s := broken.Stats(); s.FallbackRender == 0 || s.RenderError == 0 {
		t.Fatalf("the fallback is counted: %+v", s)
	}
}

// A stored template that no longer validates (e.g. edited directly in the database)
// falls back to the default instead of publishing something malformed.
func TestEmbedRuntimeMalformedStoredTemplateFallsBack(t *testing.T) {
	w := newRuntimeWorld(t, time.Hour)
	if _, err := w.a.DB.Pool.Exec(context.Background(),
		`INSERT INTO installation_embed_templates(installation_id, route_key, config_json) VALUES($1,'ECONOMY', $2::jsonb)`,
		w.fixture.InstallationID, `{"version":1,"enabled":true,"color":"not-a-color","title":{"enabled":true,"template":"{{killer}} {{"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(context.Background(),
		`INSERT INTO installation_embed_templates(installation_id, route_key, config_json) VALUES($1,'ECONOMY', '{"title": 12}'::jsonb)
		 ON CONFLICT (installation_id, route_key) DO UPDATE SET config_json = EXCLUDED.config_json`, w.fixture.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.adminCredit(w.hunter, 8, ""); err != nil {
		t.Fatal(err)
	}
	if c := w.publish("eco-A"); len(c) != 1 || c[0] != "➕ **ADMIN CREDIT**\nHunter received 8 pts" {
		t.Fatalf("a malformed stored template must fall back: %q", c)
	}
	if s := w.renderer.Stats(); s.FallbackRender != 1 || s.CustomRender != 0 {
		t.Fatalf("stats: %+v", s)
	}
}

// BOUNTY_TRACKING lifecycle cards through the real renderer + tracker.
func TestEmbedRuntimeBountyTrackingLifecycle(t *testing.T) {
	w := newRuntimeWorld(t, time.Hour)
	tr := discord.NewBountyTracker(w.rich, w.a.ChannelRoutes, w.servers)
	tr.SetCustomizer(w.renderer, serverName)
	w.a.BountyService.SetNotifier(discord.BountyEvents{Tracker: tr, Board: w.board})

	tmpl := map[string]any{
		"enabled": true, "color": "#9e4b4b",
		"title":       map[string]any{"enabled": true, "template": "Bounty {{status}}: {{target}}"},
		"description": map[string]any{"enabled": true, "template": "{{amount}} pts on {{server_name}}"},
		"fields": []map[string]any{
			{"key": "hunter", "label": "Hunter", "enabled": true, "template": "{{hunter}}", "inline": true, "order": 0},
			{"key": "weapon", "label": "Weapon", "enabled": true, "template": "{{weapon}}", "inline": true, "order": 1},
		},
	}
	if rr := w.admin.put(w.fixture.OrgID, w.fixture.InstallationID, "BOUNTY_TRACKING", w.fixture.OwnerDiscordID, tmpl); rr.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	w.save(w.fixture.InstallationID, "kf-A", map[string]string{"BOUNTY_TRACKING": "track-A"})

	if _, err := w.a.BountyService.Place(w.ctx, bounties.PlaceRequest{GuildID: w.guildRowID, ServerID: w.serverA, TargetPlayerID: w.target, Amount: 250000, PlacedBy: "admin-1"}); err != nil {
		t.Fatal(err)
	}
	tr.Flush()
	cards := w.rich.in("track-A")
	if len(cards) != 1 || !strings.Contains(cards[0], "Bounty placed: Target") || !strings.Contains(cards[0], "250,000 pts on Server-") {
		t.Fatalf("the placed card must render the template: %q", cards)
	}
	if strings.Contains(cards[0], "Hunter") || strings.Contains(cards[0], "Weapon") {
		t.Fatalf("claim-only fields must be omitted on a placement (never 'N/A'): %q", cards[0])
	}
}
