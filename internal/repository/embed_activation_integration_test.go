//go:build integration

package repository

import (
	"testing"

	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
)

// Embed runtime activation against real PostgreSQL: the saved template is used only when the
// installation selected Custom Embed for the route; the selection is per installation; reset
// returns the route to the Champion default; templates are never modified by activation.
func TestEmbedActivationGatesTheRuntimeTemplatePerInstallation(t *testing.T) {
	w := newEmbedWorld(t)
	org, inst := w.installation()
	instA := w.secondInstallation(org, inst) // server A
	instB := w.secondInstallation(org, inst) // server B, same guild and organization
	resolve := func(instID int64) (*embedtemplates.Config, int64) {
		var guildRow, server int64
		w.must(w.pool.QueryRow(w.ctx, `SELECT c.guild_id, i.game_server_id FROM installations i JOIN discord_guild_connections c ON c.id=i.discord_guild_connection_id WHERE i.id=$1`, instID).Scan(&guildRow, &server))
		got, cfg, err := w.repo.ResolveTemplate(w.ctx, guildRow, server, "KILLFEED")
		w.must(err)
		return cfg, got
	}
	_, err := w.repo.Upsert(w.ctx, org, instA, sampleConfig("KILLFEED", "A-design"))
	w.must(err)
	_, err = w.repo.Upsert(w.ctx, org, instB, sampleConfig("KILLFEED", "B-design"))
	w.must(err)

	// Saved but Champion Default selected (no row): the runtime keeps the default card.
	if cfg, got := resolve(instA); cfg != nil || got != instA {
		t.Fatalf("a saved template is not used until Custom Embed is selected: %+v", cfg)
	}
	// A selects Custom Embed; B is unaffected.
	w.must(w.repo.SetActivation(w.ctx, org, instA, "KILLFEED", EmbedModeCustom, 42))
	if cfg, _ := resolve(instA); cfg == nil || cfg.Title.Template != "A-design" {
		t.Fatalf("A's custom template is live: %+v", cfg)
	}
	if cfg, _ := resolve(instB); cfg != nil {
		t.Fatalf("one installation's activation never affects another: %+v", cfg)
	}
	modes, err := w.repo.ListActivations(w.ctx, org, instA)
	if err != nil || modes["KILLFEED"] != EmbedModeCustom || len(modes) != 1 {
		t.Fatalf("selection stored per installation and route: %v %v", modes, err)
	}
	// Idempotent activation, then Champion Default restores the default card, template kept.
	w.must(w.repo.SetActivation(w.ctx, org, instA, "KILLFEED", EmbedModeCustom, 42))
	w.must(w.repo.SetActivation(w.ctx, org, instA, "KILLFEED", EmbedModeDefault, 42))
	if cfg, _ := resolve(instA); cfg != nil {
		t.Fatal("Champion Default restores the default card")
	}
	if s, err := w.repo.Get(w.ctx, org, instA, "KILLFEED"); err != nil || s == nil || s.Config.Title.Template != "A-design" {
		t.Fatalf("deactivation never deletes or changes the saved template: %+v %v", s, err)
	}
	// Reset (delete template) also clears a Custom selection.
	w.must(w.repo.SetActivation(w.ctx, org, instA, "KILLFEED", EmbedModeCustom, 42))
	if _, err := w.repo.Delete(w.ctx, org, instA, "KILLFEED"); err != nil {
		t.Fatal(err)
	}
	if modes, _ := w.repo.ListActivations(w.ctx, org, instA); len(modes) != 0 {
		t.Fatalf("reset returns the route to Champion Default: %v", modes)
	}
	// Another organization can neither read nor write this installation's selection.
	orgX, _ := w.installation()
	if err := w.repo.SetActivation(w.ctx, orgX, instB, "KILLFEED", EmbedModeCustom, 1); err != embedtemplates.ErrInstallationNotFound {
		t.Fatalf("cross-organization activation must be refused: %v", err)
	}
	if modes, _ := w.repo.ListActivations(w.ctx, orgX, instB); len(modes) != 0 {
		t.Fatalf("cross-organization read: %v", modes)
	}
	// The selection rows cascade with their installation.
	var n int
	w.must(w.pool.QueryRow(w.ctx, `SELECT COUNT(*) FROM installation_embed_activation WHERE installation_id=$1`, instB).Scan(&n))
	if n != 0 {
		t.Fatalf("B never selected anything: %d", n)
	}
}
