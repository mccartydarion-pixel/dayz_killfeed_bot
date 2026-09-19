//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
)

type embedWorld struct {
	t    *testing.T
	pool *pgxpool.Pool
	ctx  context.Context
	tag  string
	repo *EmbedTemplateRepository
	n    int
}

func newEmbedWorld(t *testing.T) *embedWorld {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required for integration suite")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for an explicit non-production integration database")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return &embedWorld{t: t, pool: db.Pool, ctx: ctx, tag: fmt.Sprintf("emb%d", time.Now().UnixNano()), repo: NewEmbedTemplateRepository(db.Pool)}
}

func (w *embedWorld) must(err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
}

// installation seeds an organization with one installation (own guild connection),
// returning both ids.
func (w *embedWorld) installation() (orgID, instID int64) {
	w.t.Helper()
	w.n++
	name := fmt.Sprintf("%s-%d", w.tag, w.n)
	var userID, guildRow, connID int64
	w.must(w.pool.QueryRow(w.ctx, `INSERT INTO app_users(discord_user_id, discord_username) VALUES($1,$1) RETURNING id`, name).Scan(&userID))
	w.must(w.pool.QueryRow(w.ctx, `INSERT INTO organizations(name, slug, owner_user_id) VALUES($1,$1,$2) RETURNING id`, name, userID).Scan(&orgID))
	w.must(w.pool.QueryRow(w.ctx, `INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, name).Scan(&guildRow))
	w.must(w.pool.QueryRow(w.ctx, `INSERT INTO discord_guild_connections(organization_id, guild_id, guild_name) VALUES($1,$2,'g') RETURNING id`, orgID, guildRow).Scan(&connID))
	w.must(w.pool.QueryRow(w.ctx, `INSERT INTO installations(organization_id, discord_guild_connection_id, status) VALUES($1,$2,'READY') RETURNING id`, orgID, connID).Scan(&instID))
	return
}

// secondInstallation adds another installation to the SAME organization and guild
// (a second DayZ server on one guild).
func (w *embedWorld) secondInstallation(orgID, firstInst int64) int64 {
	w.t.Helper()
	var connID, serverGuild int64
	w.must(w.pool.QueryRow(w.ctx, `SELECT discord_guild_connection_id FROM installations WHERE id=$1`, firstInst).Scan(&connID))
	w.must(w.pool.QueryRow(w.ctx, `SELECT guild_id FROM discord_guild_connections WHERE id=$1`, connID).Scan(&serverGuild))
	w.n++
	var serverID, instID int64
	w.must(w.pool.QueryRow(w.ctx, `INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, display_name, status, organization_id) VALUES($1,'NITRADO',$2,'DAYZ','PLAYSTATION','S2','ACTIVE',$3) RETURNING id`,
		serverGuild, fmt.Sprintf("svc-%s-%d", w.tag, w.n), orgID).Scan(&serverID))
	w.must(w.pool.QueryRow(w.ctx, `INSERT INTO installations(organization_id, discord_guild_connection_id, game_server_id, status) VALUES($1,$2,$3,'READY') RETURNING id`, orgID, connID, serverID).Scan(&instID))
	return instID
}

func sampleConfig(route, title string) embedtemplates.Config {
	v := "{{" + embedtemplates.Variables(route)[0] + "}}" // a variable every route offers is not guaranteed; use its own first
	cfg, err := embedtemplates.Validate(embedtemplates.Config{
		Enabled: true, Color: "#d4af37",
		Title:       embedtemplates.Text{Enabled: true, Template: title},
		Description: embedtemplates.Text{Enabled: true, Template: v},
		Author:      embedtemplates.Author{Enabled: true, Name: "Champion", IconURL: "https://example.com/a.png"},
		Thumbnail:   embedtemplates.Media{Enabled: true, URL: "https://example.com/t.png"},
		Image:       embedtemplates.Media{Enabled: false},
		Footer:      embedtemplates.Footer{Enabled: true, Text: "Footer é🙂", IconURL: "https://example.com/f.png"},
		Timestamp:   true,
		Fields: []embedtemplates.Field{
			{Key: "b", Label: "Second", Enabled: true, Template: v, Inline: true, Order: 1},
			{Key: "a", Label: "First", Enabled: true, Template: "x", Inline: false, Order: 0},
		},
	}, route)
	if err != nil {
		panic(err)
	}
	return cfg
}

func (w *embedWorld) rowCount(inst int64) (n int) {
	w.must(w.pool.QueryRow(w.ctx, `SELECT COUNT(*) FROM installation_embed_templates WHERE installation_id=$1`, inst).Scan(&n))
	return
}

func TestEmbedTemplateMigrationSchema(t *testing.T) {
	w := newEmbedWorld(t)
	_, inst := w.installation()
	var typ string
	w.must(w.pool.QueryRow(w.ctx, `SELECT data_type FROM information_schema.columns WHERE table_name='installation_embed_templates' AND column_name='config_json'`).Scan(&typ))
	if typ != "jsonb" {
		t.Fatalf("config_json must be JSONB, got %s", typ)
	}
	// The migration is recorded and re-applicable (IF NOT EXISTS).
	var applied int
	w.must(w.pool.QueryRow(w.ctx, `SELECT COUNT(*) FROM schema_migrations WHERE name='0031_installation_embed_templates'`).Scan(&applied))
	if applied != 1 {
		t.Fatalf("migration must be recorded once, got %d", applied)
	}
	// The database refuses a non-object config and an oversized one, whatever the code does.
	if _, err := w.pool.Exec(w.ctx, `INSERT INTO installation_embed_templates(installation_id, route_key, config_json) VALUES($1,'KILLFEED','[1]')`, inst); err == nil {
		t.Fatal("a non-object config_json must be refused")
	}
	if _, err := w.pool.Exec(w.ctx, `INSERT INTO installation_embed_templates(installation_id, route_key, config_json) VALUES($1,'KILLFEED', jsonb_build_object('x', repeat('a', 70000)))`, inst); err == nil {
		t.Fatal("an oversized config_json must be refused")
	}
}

func TestEmbedTemplateUniqueInstallationRoute(t *testing.T) {
	w := newEmbedWorld(t)
	_, inst := w.installation()
	insert := `INSERT INTO installation_embed_templates(installation_id, route_key, config_json) VALUES($1,$2,'{}')`
	w.must(func() error { _, err := w.pool.Exec(w.ctx, insert, inst, "KILLFEED"); return err }())
	if _, err := w.pool.Exec(w.ctx, insert, inst, "KILLFEED"); err == nil {
		t.Fatal("(installation, route) must be unique")
	}
	if _, err := w.pool.Exec(w.ctx, insert, inst, "HITFEED"); err != nil {
		t.Fatalf("another route on the same installation is fine: %v", err)
	}
	if _, err := w.pool.Exec(w.ctx, insert, 987654321, "KILLFEED"); err == nil {
		t.Fatal("the installation foreign key must be enforced")
	}
}

func TestEmbedTemplateUpsertPreservesCreatedAtAndRefreshesUpdatedAt(t *testing.T) {
	w := newEmbedWorld(t)
	org, inst := w.installation()
	first, err := w.repo.Upsert(w.ctx, org, inst, sampleConfig("KILLFEED", "one {{killer}}"))
	w.must(err)
	time.Sleep(30 * time.Millisecond)
	second, err := w.repo.Upsert(w.ctx, org, inst, sampleConfig("KILLFEED", "two {{victim}}"))
	w.must(err)

	if second.ID != first.ID || w.rowCount(inst) != 1 {
		t.Fatalf("an upsert must update the existing row: ids %d/%d rows %d", first.ID, second.ID, w.rowCount(inst))
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("created_at must be preserved: %v -> %v", first.CreatedAt, second.CreatedAt)
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Fatalf("updated_at must advance: %v -> %v", first.UpdatedAt, second.UpdatedAt)
	}
	if second.Config.Title.Template != "two {{victim}}" {
		t.Fatalf("the new content must replace the old: %+v", second.Config.Title)
	}
	got, err := w.repo.Get(w.ctx, org, inst, "KILLFEED")
	w.must(err)
	if got == nil || got.Config.Title.Template != "two {{victim}}" || !got.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("a read must return the latest write: %+v", got)
	}
}

// JSONB round-trip: every valid value, including unicode, ordering and the empty
// optional sections, comes back identical.
func TestEmbedTemplateJSONBRoundTrip(t *testing.T) {
	w := newEmbedWorld(t)
	org, inst := w.installation()
	for _, route := range []string{"KILLFEED", "ECONOMY", "BOUNTY_TRACKING"} {
		want := sampleConfig(route, "héllo 🙂 {{server_name}}")
		if _, err := w.repo.Upsert(w.ctx, org, inst, want); err != nil {
			t.Fatal(err)
		}
		got, err := w.repo.Get(w.ctx, org, inst, route)
		w.must(err)
		if got == nil || !reflect.DeepEqual(got.Config, want) {
			gj, _ := json.Marshal(got.Config)
			wj, _ := json.Marshal(want)
			t.Fatalf("%s did not round-trip:\n got %s\nwant %s", route, gj, wj)
		}
	}
	// An empty fields list stays a list (never null) after the round-trip.
	empty := sampleConfig("HITFEED", "x")
	empty.Fields = []embedtemplates.Field{}
	if _, err := w.repo.Upsert(w.ctx, org, inst, empty); err != nil {
		t.Fatal(err)
	}
	got, _ := w.repo.Get(w.ctx, org, inst, "HITFEED")
	if got.Config.Fields == nil || len(got.Config.Fields) != 0 {
		t.Fatalf("empty fields must round-trip as an empty list: %#v", got.Config.Fields)
	}
}

func TestEmbedTemplateRoutesAndInstallationsAreIndependent(t *testing.T) {
	w := newEmbedWorld(t)
	org, inst := w.installation()
	inst2 := w.secondInstallation(org, inst) // same organization AND same guild, second server

	for _, c := range []struct {
		inst  int64
		route string
		title string
	}{{inst, "KILLFEED", "A-kill"}, {inst, "HITFEED", "A-hit"}, {inst2, "KILLFEED", "B-kill"}} {
		if _, err := w.repo.Upsert(w.ctx, org, c.inst, sampleConfig(c.route, c.title)); err != nil {
			t.Fatal(err)
		}
	}
	titleOf := func(i int64, route string) string {
		s, err := w.repo.Get(w.ctx, org, i, route)
		w.must(err)
		if s == nil {
			return "<none>"
		}
		return s.Config.Title.Template
	}
	if titleOf(inst, "KILLFEED") != "A-kill" || titleOf(inst, "HITFEED") != "A-hit" || titleOf(inst2, "KILLFEED") != "B-kill" || titleOf(inst2, "HITFEED") != "<none>" {
		t.Fatal("routes and installations must be independent")
	}
	// Changing installation A's KILLFEED must not touch installation B's.
	if _, err := w.repo.Upsert(w.ctx, org, inst, sampleConfig("KILLFEED", "A-kill-v2")); err != nil {
		t.Fatal(err)
	}
	if titleOf(inst2, "KILLFEED") != "B-kill" || titleOf(inst, "KILLFEED") != "A-kill-v2" {
		t.Fatal("editing one installation must not affect another on the same guild")
	}
	// Deleting one route leaves the others.
	existed, err := w.repo.Delete(w.ctx, org, inst, "KILLFEED")
	w.must(err)
	if !existed || titleOf(inst, "KILLFEED") != "<none>" || titleOf(inst, "HITFEED") != "A-hit" || titleOf(inst2, "KILLFEED") != "B-kill" {
		t.Fatal("delete must remove only that installation+route")
	}
	list, err := w.repo.List(w.ctx, org, inst)
	w.must(err)
	if len(list) != 1 || list[0].Config.RouteKey != "HITFEED" {
		t.Fatalf("list must show only this installation's remaining template: %+v", list)
	}
	empty, err := w.repo.List(w.ctx, org, w.secondInstallation(org, inst))
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("an installation with no templates lists an empty (non-nil) slice: %v %v", empty, err)
	}
}

func TestEmbedTemplateDeleteIsIdempotent(t *testing.T) {
	w := newEmbedWorld(t)
	org, inst := w.installation()
	if existed, err := w.repo.Delete(w.ctx, org, inst, "KILLFEED"); err != nil || existed {
		t.Fatalf("deleting nothing is not an error: %v %v", existed, err)
	}
	if _, err := w.repo.Upsert(w.ctx, org, inst, sampleConfig("KILLFEED", "x")); err != nil {
		t.Fatal(err)
	}
	if existed, err := w.repo.Delete(w.ctx, org, inst, "KILLFEED"); err != nil || !existed {
		t.Fatalf("first delete: %v %v", existed, err)
	}
	if existed, err := w.repo.Delete(w.ctx, org, inst, "KILLFEED"); err != nil || existed {
		t.Fatalf("second delete: %v %v", existed, err)
	}
	if got, err := w.repo.Get(w.ctx, org, inst, "KILLFEED"); err != nil || got != nil {
		t.Fatalf("expected nothing after reset: %v %v", got, err)
	}
}

// The organization is part of every query: an installation id alone selects nothing.
func TestEmbedTemplateCrossOrganizationIsolation(t *testing.T) {
	w := newEmbedWorld(t)
	orgA, instA := w.installation()
	orgB, instB := w.installation()
	if _, err := w.repo.Upsert(w.ctx, orgA, instA, sampleConfig("KILLFEED", "A-secret-title")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.repo.Upsert(w.ctx, orgB, instB, sampleConfig("KILLFEED", "B-title")); err != nil {
		t.Fatal(err)
	}

	// Org B naming org A's installation: cannot read, list or delete.
	if got, err := w.repo.Get(w.ctx, orgB, instA, "KILLFEED"); err != nil || got != nil {
		t.Fatalf("org B must not read org A's template: %v %v", got, err)
	}
	if list, err := w.repo.List(w.ctx, orgB, instA); err != nil || len(list) != 0 {
		t.Fatalf("org B must not list org A's templates: %v %v", list, err)
	}
	if existed, err := w.repo.Delete(w.ctx, orgB, instA, "KILLFEED"); err != nil || existed {
		t.Fatalf("org B must not delete org A's template: %v %v", existed, err)
	}
	// ...and cannot write: nothing is created and nothing is overwritten.
	if _, err := w.repo.Upsert(w.ctx, orgB, instA, sampleConfig("KILLFEED", "HIJACK")); !errors.Is(err, embedtemplates.ErrInstallationNotFound) {
		t.Fatalf("org B writing to org A's installation must be refused, got %v", err)
	}
	if _, err := w.repo.Upsert(w.ctx, orgB, instA, sampleConfig("HITFEED", "HIJACK")); !errors.Is(err, embedtemplates.ErrInstallationNotFound) {
		t.Fatalf("a new route on a foreign installation must be refused too, got %v", err)
	}
	a, err := w.repo.Get(w.ctx, orgA, instA, "KILLFEED")
	w.must(err)
	if a == nil || a.Config.Title.Template != "A-secret-title" || w.rowCount(instA) != 1 {
		t.Fatalf("org A's data must be untouched: %+v rows=%d", a, w.rowCount(instA))
	}
	// A nonexistent installation is the same refusal.
	if _, err := w.repo.Upsert(w.ctx, orgA, 987654321, sampleConfig("KILLFEED", "x")); !errors.Is(err, embedtemplates.ErrInstallationNotFound) {
		t.Fatalf("unknown installation: %v", err)
	}
}

// Simultaneous saves to one (installation, route) converge on ONE row holding one of
// the written values in full - never a mixture, never a duplicate, never an error.
func TestEmbedTemplateConcurrentUpsertsProduceOneRow(t *testing.T) {
	w := newEmbedWorld(t)
	org, inst := w.installation()
	const workers = 30
	titles := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		title := fmt.Sprintf("writer-%02d", i)
		titles[title] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			cfg := sampleConfig("KILLFEED", title)
			cfg.Footer.Text = "footer of " + title // a second field that must stay consistent with the title
			if _, err := w.repo.Upsert(w.ctx, org, inst, cfg); err != nil {
				mu.Lock()
				t.Errorf("concurrent upsert failed: %v", err)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := w.rowCount(inst); n != 1 {
		t.Fatalf("expected exactly one row, got %d", n)
	}
	got, err := w.repo.Get(w.ctx, org, inst, "KILLFEED")
	w.must(err)
	if got == nil || !titles[got.Config.Title.Template] || got.Config.Footer.Text != "footer of "+got.Config.Title.Template {
		t.Fatalf("the final row must be one writer's complete config: %+v", got)
	}
}

func TestEmbedTemplatesAreDeletedWithTheirInstallation(t *testing.T) {
	w := newEmbedWorld(t)
	org, inst := w.installation()
	keep := w.secondInstallation(org, inst)
	for _, i := range []int64{inst, keep} {
		if _, err := w.repo.Upsert(w.ctx, org, i, sampleConfig("KILLFEED", "x")); err != nil {
			t.Fatal(err)
		}
	}
	w.must(func() error { _, err := w.pool.Exec(w.ctx, `DELETE FROM installations WHERE id=$1`, inst); return err }())
	if w.rowCount(inst) != 0 {
		t.Fatal("deleting an installation must delete its templates (existing child-row semantics)")
	}
	if w.rowCount(keep) != 1 {
		t.Fatal("another installation's templates must remain")
	}
}
