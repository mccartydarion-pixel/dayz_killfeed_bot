//go:build integration

package app

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/embedrender"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// cardCapture is the KILLFEED consumer of one server's worker: the engine hands it each kill
// AFTER durable persistence, and it records the card the real KillfeedPublisher builds (the
// default, the custom card, or the fallback) - the same call PublishKill makes.
type cardCapture struct {
	p     *discord.KillfeedPublisher
	mu    sync.Mutex
	cards []*discordgo.MessageEmbed
	evs   []*killfeed.Event
}

func (c *cardCapture) PublishKill(ev *killfeed.Event) error {
	card := c.p.Card(ev)
	c.mu.Lock()
	c.cards = append(c.cards, card)
	c.evs = append(c.evs, ev)
	c.mu.Unlock()
	return nil
}

func (c *cardCapture) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.cards) }
func (c *cardCapture) last() (*discordgo.MessageEmbed, *killfeed.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cards[len(c.cards)-1], c.evs[len(c.evs)-1]
}

// server wires one game server's worker exactly as app.go does: a persistence queue that
// claims bounties and publishes kills, and a KillfeedPublisher bound to (guild, server) with
// the renderer (nil = the flag is off: no customizer is wired at all).
func (w *runtimeWorld) server(serverID int64, r *embedrender.Renderer) (*killfeed.PersistenceQueue, *cardCapture) {
	p := discord.NewKillfeedPublisher(nil, "")
	p.SetRouting(nil, w.guildRowID, serverID)
	if r != nil {
		p.SetCustomizer(r, serverName(serverID))
	}
	cap := &cardCapture{p: p}
	q := killfeed.NewPersistenceQueueWithServerID(w.adapter, w.guildRowID, serverID, "session")
	q.SetKillPostProcessor(w.adapter)
	go q.Run(w.ctx)
	engine := killfeed.NewEngine(nil, "svc", killfeed.NewADMParser())
	engine.SetPersistence(q)
	engine.SetKillPublisher(cap)
	return q, cap
}

func (w *runtimeWorld) kill(q *killfeed.PersistenceQueue, tod string) {
	w.t.Helper()
	if err := q.EnqueueAndWait(w.ctx, killOf(tod)); err != nil {
		w.t.Fatal(err)
	}
}

func (w *runtimeWorld) killsPersisted(serverID int64) (n int) {
	w.t.Helper()
	must(w.t, w.a.DB.Pool.QueryRow(w.ctx, `SELECT COUNT(*) FROM kills WHERE guild_id=$1 AND server_id=$2`, w.guildRowID, serverID).Scan(&n))
	return
}

func killTemplateBody(style string) map[string]any {
	return map[string]any{
		"enabled": true, "color": "#d4af37",
		"title":       map[string]any{"enabled": true, "template": style + " {{killer}} eliminated {{victim}}"},
		"description": map[string]any{"enabled": true, "template": "{{weapon}} from {{distance}} on {{server_name}}"},
		"footer":      map[string]any{"enabled": true, "text": "Champion Killfeed", "iconUrl": ""},
		"timestamp":   true,
		"fields": []map[string]any{
			{"key": "ammo", "label": "Ammo", "enabled": true, "template": "{{ammo}}", "inline": true, "order": 0},
			{"key": "distance", "label": "Distance", "enabled": true, "template": "{{distance}}", "inline": true, "order": 1},
			{"key": "streak", "label": "Streak", "enabled": true, "template": "{{streak}}", "inline": true, "order": 2},
		},
	}
}

func (w *runtimeWorld) putKill(inst int64, style string) {
	w.t.Helper()
	if rr := w.admin.put(w.fixture.OrgID, inst, "KILLFEED", w.fixture.OwnerDiscordID, killTemplateBody(style)); rr.Code != http.StatusOK {
		w.t.Fatalf("save KILLFEED template: %d %s", rr.Code, rr.Body.String())
	}
}

func isDefaultKillCard(card *discordgo.MessageEmbed, ev *killfeed.Event) bool {
	return reflect.DeepEqual(card, discord.BuildKillEmbed(ev))
}

// The whole path against real PostgreSQL: a kill is persisted, then its card is the custom
// template; after DELETE the next kill is the untouched Champion card. One kill = one card.
func TestKillfeedCustomTemplateEndToEnd(t *testing.T) {
	w := newRuntimeWorld(t, time.Hour) // only invalidation can explain immediacy
	q, cap := w.server(w.serverA, w.renderer)

	// No customization: the existing Champion kill card.
	w.kill(q, "10:00:01")
	card, ev := cap.last()
	if !isDefaultKillCard(card, ev) || cap.count() != 1 {
		t.Fatalf("no template = the existing card, exactly one: %+v", card)
	}

	// Custom template saved through the real API.
	w.putKill(w.fixture.InstallationID, "STYLE-A")
	w.kill(q, "10:00:02")
	card, ev = cap.last()
	if cap.count() != 2 || isDefaultKillCard(card, ev) {
		t.Fatalf("expected the custom card, cards=%d", cap.count())
	}
	if card.Title != "STYLE-A Hunter eliminated Target" || !strings.HasPrefix(card.Description, "M4-A1 from 86.4m on Server-") || card.Color != 0xD4AF37 {
		t.Fatalf("custom card content: %q / %q / %x", card.Title, card.Description, card.Color)
	}
	// This kill carries no ammo: that field is omitted (never "N/A"). The streak IS carried
	// (the hunter's second kill), so its field shows the authoritative value.
	fields := map[string]string{}
	for _, f := range card.Fields {
		if strings.Contains(f.Value, "N/A") {
			t.Fatalf("no placeholder values: %+v", f)
		}
		fields[f.Name] = f.Value
	}
	if _, ok := fields["Ammo"]; ok || fields["Distance"] != "86.4m" || fields["Streak"] != "2" {
		t.Fatalf("optional fields: %v", fields)
	}
	if card.Timestamp == "" || card.Footer == nil || card.Footer.Text != "Champion Killfeed" {
		t.Fatalf("timestamp/footer: %+v", card)
	}

	// Persistence and dedupe are untouched: both kills stored, a replay adds nothing.
	if got := w.killsPersisted(w.serverA); got != 2 {
		t.Fatalf("two kills must be persisted, got %d", got)
	}
	before := cap.count()
	w.kill(q, "10:00:02") // the same ADM line again
	if cap.count() != before || w.killsPersisted(w.serverA) != 2 {
		t.Fatalf("a replayed kill must be neither stored nor published again: cards %d->%d rows=%d", before, cap.count(), w.killsPersisted(w.serverA))
	}

	// Reset: the Champion card is back on the very next kill.
	if rr := w.admin.del(w.fixture.OrgID, w.fixture.InstallationID, "KILLFEED", w.fixture.OwnerDiscordID); rr.Code != http.StatusOK {
		t.Fatalf("reset: %d", rr.Code)
	}
	w.kill(q, "10:00:03")
	card, ev = cap.last()
	if !isDefaultKillCard(card, ev) {
		t.Fatalf("after DELETE the default card must return: %+v", card)
	}
}

// Same Discord guild, two servers: A's template must not touch B, and B is independently
// customizable afterwards.
func TestKillfeedMultiInstallationIsolation(t *testing.T) {
	w := newRuntimeWorld(t, time.Hour)
	qa, capA := w.server(w.serverA, w.renderer)
	qb, capB := w.server(w.serverB, w.renderer)

	w.putKill(w.fixture.InstallationID, "ONLY-A")
	w.kill(qa, "11:00:01")
	w.kill(qb, "11:10:01") // distinct times: the kill fingerprint is per guild
	cardA, _ := capA.last()
	cardB, evB := capB.last()
	if cardA.Title != "ONLY-A Hunter eliminated Target" {
		t.Fatalf("server A uses its template: %q", cardA.Title)
	}
	if !isDefaultKillCard(cardB, evB) {
		t.Fatalf("server B (same guild) must keep the Champion card: %+v", cardB)
	}

	w.putKill(w.installB, "ONLY-B")
	w.kill(qa, "11:00:02")
	w.kill(qb, "11:10:02")
	cardA, _ = capA.last()
	cardB, _ = capB.last()
	if cardA.Title != "ONLY-A Hunter eliminated Target" || cardB.Title != "ONLY-B Hunter eliminated Target" {
		t.Fatalf("each server keeps its own style: %q / %q", cardA.Title, cardB.Title)
	}
	// Resetting B leaves A alone.
	if rr := w.admin.del(w.fixture.OrgID, w.installB, "KILLFEED", w.fixture.OwnerDiscordID); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	w.kill(qa, "11:00:03")
	w.kill(qb, "11:10:03")
	cardA, _ = capA.last()
	cardB, evB = capB.last()
	if cardA.Title != "ONLY-A Hunter eliminated Target" || !isDefaultKillCard(cardB, evB) {
		t.Fatalf("resetting B must not affect A: %q / default=%v", cardA.Title, isDefaultKillCard(cardB, evB))
	}
}

// A failing template lookup costs neither the kill nor its card: it persists, one default
// card is produced, nothing crashes.
func TestKillfeedTemplateLookupFailureFallsBackWithoutLosingTheKill(t *testing.T) {
	w := newRuntimeWorld(t, time.Hour)
	w.putKill(w.fixture.InstallationID, "STYLE")
	brokenPool, err := pgxpool.New(context.Background(), w.a.DB.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	brokenPool.Close() // every lookup through it fails
	broken := embedrender.New(embedrender.Options{Source: repository.NewEmbedTemplateRepository(brokenPool), Enabled: true})
	q, cap := w.server(w.serverA, broken)

	w.kill(q, "12:00:01")
	w.kill(q, "12:00:02")
	if cap.count() != 2 || w.killsPersisted(w.serverA) != 2 {
		t.Fatalf("both kills must persist and publish once each: cards=%d rows=%d", cap.count(), w.killsPersisted(w.serverA))
	}
	for i := range cap.cards {
		if !isDefaultKillCard(cap.cards[i], cap.evs[i]) {
			t.Fatalf("card %d must be the Champion default", i)
		}
	}
	if s := broken.Stats(); s.FallbackRender == 0 || s.ByRoute["KILLFEED"].Fallback == 0 {
		t.Fatalf("the fallback is counted per route: %+v", s)
	}
}

// The rollout flag: off = every saved template is ignored at runtime and nothing in the
// database changes; on = the same saved template renders.
func TestKillfeedRolloutFlag(t *testing.T) {
	w := newRuntimeWorld(t, time.Hour)
	w.putKill(w.fixture.InstallationID, "FLAGGED")
	rowsBefore := templateRows(t, w)

	off := embedrender.New(embedrender.Options{Source: repository.NewEmbedTemplateRepository(w.a.DB.Pool), Enabled: false})
	qOff, capOff := w.server(w.serverA, off) // even if wired, a disabled renderer changes nothing
	w.kill(qOff, "13:00:01")
	card, ev := capOff.last()
	if !isDefaultKillCard(card, ev) || off.Stats().CustomRender != 0 {
		t.Fatalf("flag off: the saved template must be ignored: %+v", card)
	}
	// The app wires NO customizer at all when the flag is off.
	if (&App{EmbedRenderer: off}).embedCustomizer() != nil {
		t.Fatal("flag off: publishers must not be wired to the renderer")
	}

	qOn, capOn := w.server(w.serverA, w.renderer)
	w.kill(qOn, "13:00:02")
	card, _ = capOn.last()
	if card.Title != "FLAGGED Hunter eliminated Target" {
		t.Fatalf("flag on: the same saved template renders: %q", card.Title)
	}
	if !reflect.DeepEqual(rowsBefore, templateRows(t, w)) {
		t.Fatal("flipping the flag must not change stored templates")
	}
	if (&App{EmbedRenderer: w.renderer}).embedCustomizer() == nil {
		t.Fatal("flag on: publishers are wired to the renderer")
	}
}

func templateRows(t *testing.T, w *runtimeWorld) []string {
	t.Helper()
	rows, err := w.a.DB.Pool.Query(w.ctx, `SELECT installation_id::text || '/' || route_key || '/' || config_json::text FROM installation_embed_templates WHERE installation_id IN ($1,$2) ORDER BY 1`, w.fixture.InstallationID, w.installB)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// Custom BOUNTY_TRACKING presentation must not touch the claim: the kill still claims once,
// awards the points once, stacks the totals, and the KILLFEED bounty story is unchanged.
func TestBountyClaimBehaviorUnchangedByCustomTemplates(t *testing.T) {
	w := newRuntimeWorld(t, time.Hour)
	tr := discord.NewBountyTracker(w.rich, w.a.ChannelRoutes, w.servers)
	tr.SetCustomizer(w.renderer, serverName)
	w.a.BountyService.SetNotifier(discord.BountyEvents{Tracker: tr, Board: w.board})
	w.save(w.fixture.InstallationID, "kf-A", map[string]string{"BOUNTY_TRACKING": "track-A"})
	tmpl := map[string]any{"enabled": true, "color": "#9e4b4b",
		"title":       map[string]any{"enabled": true, "template": "TRACK {{status}} {{target}}"},
		"description": map[string]any{"enabled": true, "template": "{{amount}} by {{hunter}}"}}
	if rr := w.admin.put(w.fixture.OrgID, w.fixture.InstallationID, "BOUNTY_TRACKING", w.fixture.OwnerDiscordID, tmpl); rr.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	q, cap := w.server(w.serverA, w.renderer)

	for _, amt := range []int64{100000, 150000} { // two stacked bounties
		if _, err := w.a.BountyService.Place(w.ctx, bounties.PlaceRequest{GuildID: w.guildRowID, ServerID: w.serverA, TargetPlayerID: w.target, Amount: amt, PlacedBy: "admin"}); err != nil {
			t.Fatal(err)
		}
	}
	w.kill(q, "14:00:01")
	tr.Flush()

	if life, txs := w.points(); life != 250000 || txs != 2 {
		t.Fatalf("the claim must award both stacked bounties exactly once: lifetime=%d txs=%d", life, txs)
	}
	if active, claimed := w.status(w.target); active != 0 || claimed != 2 {
		t.Fatalf("both bounties claimed: active=%d claimed=%d", active, claimed)
	}
	if cap.count() != 1 {
		t.Fatalf("one kill, one KILLFEED card, got %d", cap.count())
	}
	if _, ev := cap.last(); !ev.BountyClaimed || ev.BountyPoints != 250000 {
		t.Fatalf("the KILLFEED bounty story is unchanged: %+v", ev)
	}
	cards := w.rich.in("track-A")
	if len(cards) != 3 || !strings.Contains(cards[2], "TRACK claimed Target") || !strings.Contains(cards[2], "250,000 by Hunter") {
		t.Fatalf("the claim card renders the template: %q", cards)
	}
	// Replay: nothing more is awarded or announced.
	w.kill(q, "14:00:01")
	tr.Flush()
	if life, txs := w.points(); life != 250000 || txs != 2 || len(w.rich.in("track-A")) != 3 {
		t.Fatalf("a replay must change nothing: %d/%d cards=%d", life, txs, len(w.rich.in("track-A")))
	}
}
