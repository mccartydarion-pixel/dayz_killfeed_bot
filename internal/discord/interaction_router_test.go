package discord

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func (r *recordingTransport) snapshot() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRequest(nil), r.reqs...)
}

func (r *recordingTransport) waitFor(t *testing.T, n int) []recordedRequest {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if reqs := r.snapshot(); len(reqs) >= n {
			return reqs
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("want %d Discord requests, got %+v", n, r.snapshot())
	return nil
}

func testRouter(t *testing.T) (*InteractionRouter, *discordgo.Session, *recordingTransport) {
	t.Helper()
	s, rec := recordingSession(t)
	router := newInteractionRouter(newInteractionTimer())
	router.timer.log = func(slog.Level, ...any) {}
	router.ackAfter = 15 * time.Millisecond
	return router, s, rec
}

var testInteractionSeq = 900000

func newTestInteraction(t *testing.T, kind discordgo.InteractionType, name string) *discordgo.InteractionCreate {
	t.Helper()
	testInteractionSeq++
	id := "router-" + time.Now().Format("150405.000000") + "-" + string(rune('a'+testInteractionSeq%26)) + name
	in := &discordgo.Interaction{ID: id, AppID: "app", Token: "tok-" + id, Type: kind, GuildID: "g"}
	switch kind {
	case discordgo.InteractionApplicationCommand, discordgo.InteractionApplicationCommandAutocomplete:
		fields := strings.Fields(name)
		data := discordgo.ApplicationCommandInteractionData{Name: fields[0]}
		if len(fields) > 1 {
			data.Options = []*discordgo.ApplicationCommandInteractionDataOption{{Name: fields[1], Type: discordgo.ApplicationCommandOptionSubCommand}}
		}
		in.Data = data
	case discordgo.InteractionMessageComponent:
		in.Data = discordgo.MessageComponentInteractionData{CustomID: name}
	case discordgo.InteractionModalSubmit:
		in.Data = discordgo.ModalSubmitInteractionData{CustomID: name}
	}
	i := &discordgo.InteractionCreate{Interaction: in}
	t.Cleanup(func() { deferredReplies.Delete(id) })
	return i
}

func isCallback(r recordedRequest) bool {
	return r.method == http.MethodPost && strings.HasSuffix(r.path, "/callback")
}

func callbackData(r recordedRequest) map[string]any {
	data, _ := r.body["data"].(map[string]any)
	return data
}

// A handler that is still working when the acknowledgement deadline passes is
// deferred by the router in the route's mode, and its reply then lands in the
// right place: nothing is ever answered twice and private replies stay private.
func TestRouterDefersSlowHandlerInRegisteredMode(t *testing.T) {
	ephemeral := float64(discordgo.MessageFlagsEphemeral)
	cases := []struct {
		name  string
		ack   AckMode
		kind  discordgo.InteractionType
		check func(t *testing.T, reqs []recordedRequest)
	}{
		{"private", AckPrivate, discordgo.InteractionApplicationCommand, func(t *testing.T, reqs []recordedRequest) {
			if reqs[0].body["type"] != float64(discordgo.InteractionResponseDeferredChannelMessageWithSource) || callbackData(reqs[0])["flags"] != ephemeral {
				t.Fatalf("not a private deferral: %+v", reqs[0])
			}
			if len(reqs) != 2 || reqs[1].method != http.MethodPatch || !strings.HasSuffix(reqs[1].path, "/messages/@original") || reqs[1].body["content"] != "done" {
				t.Fatalf("reply did not fill the deferral: %+v", reqs)
			}
		}},
		{"public", AckPublic, discordgo.InteractionApplicationCommand, func(t *testing.T, reqs []recordedRequest) {
			if reqs[0].body["type"] != float64(discordgo.InteractionResponseDeferredChannelMessageWithSource) || callbackData(reqs[0])["flags"] != nil {
				t.Fatalf("not a public deferral: %+v", reqs[0])
			}
			// A private reply after a public deferral must not become public.
			if len(reqs) != 3 || reqs[1].method != http.MethodDelete || reqs[2].method != http.MethodPost || reqs[2].body["flags"] != ephemeral || reqs[2].body["content"] != "done" {
				t.Fatalf("private reply after a public deferral: %+v", reqs)
			}
		}},
		{"update", AckUpdate, discordgo.InteractionMessageComponent, func(t *testing.T, reqs []recordedRequest) {
			if reqs[0].body["type"] != float64(discordgo.InteractionResponseDeferredMessageUpdate) {
				t.Fatalf("not a deferred update: %+v", reqs[0])
			}
			if len(reqs) != 2 || reqs[1].method != http.MethodPost || isCallback(reqs[1]) || reqs[1].body["flags"] != ephemeral || reqs[1].body["content"] != "done" {
				t.Fatalf("private reply after a deferred update: %+v", reqs)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, s, rec := testRouter(t)
			handler := func(s *discordgo.Session, i *discordgo.InteractionCreate) {
				rec.waitFor(t, 1) // "the database is slow": nothing sent until the router defers
				respondEphemeral(s, i, "done")
			}
			if tc.kind == discordgo.InteractionMessageComponent {
				router.Component("slow", tc.ack, handler)
			} else {
				router.Command("slow", tc.ack, handler)
			}
			router.Dispatch(s, newTestInteraction(t, tc.kind, "slow"))
			reqs := rec.snapshot()
			if len(reqs) == 0 || !isCallback(reqs[0]) {
				t.Fatalf("first request is not the acknowledgement: %+v", reqs)
			}
			tc.check(t, reqs)
		})
	}
}

func TestRouterFastHandlerAnswersDirectly(t *testing.T) {
	router, s, rec := testRouter(t)
	router.ackAfter = time.Hour
	router.Command("fast", AckPrivate, func(s *discordgo.Session, i *discordgo.InteractionCreate) { respondEphemeral(s, i, "hi") })
	router.Dispatch(s, newTestInteraction(t, discordgo.InteractionApplicationCommand, "fast"))
	reqs := rec.snapshot()
	if len(reqs) != 1 || reqs[0].body["type"] != float64(discordgo.InteractionResponseChannelMessageWithSource) || callbackData(reqs[0])["content"] != "hi" {
		t.Fatalf("want one direct answer, got %+v", reqs)
	}
}

// A deferred message update followed by the handler's update edits the
// message the button sits on, with the same fields an immediate update sends.
func TestRouterDeferredUpdateIsEdited(t *testing.T) {
	router, s, rec := testRouter(t)
	router.Component("pay", AckUpdate, func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		rec.waitFor(t, 1)
		respondUpdate(s, i, &discordgo.InteractionResponseData{Content: "paid", Components: []discordgo.MessageComponent{}})
	})
	router.Dispatch(s, newTestInteraction(t, discordgo.InteractionMessageComponent, "pay"))
	reqs := rec.snapshot()
	if len(reqs) != 2 || reqs[0].body["type"] != float64(discordgo.InteractionResponseDeferredMessageUpdate) {
		t.Fatalf("requests: %+v", reqs)
	}
	edit := reqs[1]
	if edit.method != http.MethodPatch || !strings.HasSuffix(edit.path, "/messages/@original") || edit.body["content"] != "paid" {
		t.Fatalf("edit: %+v", edit)
	}
	if _, ok := edit.body["components"]; !ok {
		t.Fatalf("buttons were not cleared: %+v", edit.body)
	}
	if _, ok := edit.body["embeds"]; ok {
		t.Fatalf("embeds were touched although the update did not set them: %+v", edit.body)
	}
}

func TestRouterRecoversPanicAndAnswers(t *testing.T) {
	t.Run("before any answer", func(t *testing.T) {
		router, s, rec := testRouter(t)
		router.ackAfter = time.Hour
		router.Command("boom", AckPrivate, func(*discordgo.Session, *discordgo.InteractionCreate) { panic("secret user content") })
		router.Dispatch(s, newTestInteraction(t, discordgo.InteractionApplicationCommand, "boom"))
		reqs := rec.snapshot()
		if len(reqs) != 1 || callbackData(reqs[0])["content"] != interactionFailureText || callbackData(reqs[0])["flags"] != float64(discordgo.MessageFlagsEphemeral) {
			t.Fatalf("panic was not answered privately: %+v", reqs)
		}
	})
	t.Run("after deferring", func(t *testing.T) {
		router, s, rec := testRouter(t)
		router.ackAfter = time.Hour
		router.Command("boom", AckPrivate, func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			deferEphemeral(s, i)
			var none []int
			_ = none[3]
		})
		router.Dispatch(s, newTestInteraction(t, discordgo.InteractionApplicationCommand, "boom"))
		reqs := rec.snapshot()
		if len(reqs) != 2 || reqs[1].method != http.MethodPatch || reqs[1].body["content"] != interactionFailureText {
			t.Fatalf("thinking... was left unfilled after a panic: %+v", reqs)
		}
	})
	t.Run("after a full reply nothing more is sent", func(t *testing.T) {
		router, s, rec := testRouter(t)
		router.ackAfter = time.Hour
		router.Command("boom", AckPrivate, func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			deferEphemeral(s, i)
			respondEphemeral(s, i, "result")
			panic("late")
		})
		router.Dispatch(s, newTestInteraction(t, discordgo.InteractionApplicationCommand, "boom"))
		if reqs := rec.snapshot(); len(reqs) != 2 || reqs[1].body["content"] != "result" {
			t.Fatalf("a delivered reply was overwritten: %+v", reqs)
		}
	})
}

func TestRouterAnswersHandlerThatReturnsSilently(t *testing.T) {
	router, s, rec := testRouter(t)
	router.ackAfter = time.Hour
	router.Command("quiet", AckPrivate, func(*discordgo.Session, *discordgo.InteractionCreate) {})
	router.Autocomplete("quiet", func(*discordgo.Session, *discordgo.InteractionCreate) {})
	router.Dispatch(s, newTestInteraction(t, discordgo.InteractionApplicationCommand, "quiet"))
	router.Dispatch(s, newTestInteraction(t, discordgo.InteractionApplicationCommandAutocomplete, "quiet"))
	reqs := rec.snapshot()
	if len(reqs) != 2 || callbackData(reqs[0])["content"] != interactionFailureText {
		t.Fatalf("silent handler not answered: %+v", reqs)
	}
	if reqs[1].body["type"] != float64(discordgo.InteractionApplicationCommandAutocompleteResult) {
		t.Fatalf("silent autocomplete not answered with an empty list: %+v", reqs[1])
	}
}

func TestRouterAnswersInteractionsWithoutARoute(t *testing.T) {
	router, s, rec := testRouter(t)
	router.Dispatch(s, newTestInteraction(t, discordgo.InteractionApplicationCommand, "gone"))
	router.Dispatch(s, newTestInteraction(t, discordgo.InteractionMessageComponent, "old:button:1"))
	router.Dispatch(s, newTestInteraction(t, discordgo.InteractionModalSubmit, "old:form"))
	router.Dispatch(s, newTestInteraction(t, discordgo.InteractionApplicationCommandAutocomplete, "gone"))
	reqs := rec.snapshot()
	if len(reqs) != 4 {
		t.Fatalf("want every unrouted interaction answered, got %+v", reqs)
	}
	if callbackData(reqs[0])["content"] != unroutedCommandText || callbackData(reqs[1])["content"] != unroutedComponentText || callbackData(reqs[2])["content"] != unroutedComponentText {
		t.Fatalf("unrouted texts: %+v", reqs)
	}
	if reqs[3].body["type"] != float64(discordgo.InteractionApplicationCommandAutocompleteResult) {
		t.Fatalf("unrouted autocomplete: %+v", reqs[3])
	}
}

// Opening a form cannot be deferred, so the router must not defer an AckSelf
// route even when its handler is slow: a deferral would make the form fail.
func TestRouterNeverDefersAFormRoute(t *testing.T) {
	router, s, rec := testRouter(t)
	router.ackAfter = time.Millisecond
	router.Command("server", AckPrivate, func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		time.Sleep(30 * time.Millisecond)
		_ = respondModalData(s, i, &discordgo.InteractionResponseData{CustomID: "form", Title: "Form"})
	}, SubAck{Path: "connect", Ack: AckSelf})
	router.Dispatch(s, newTestInteraction(t, discordgo.InteractionApplicationCommand, "server connect"))
	reqs := rec.snapshot()
	if len(reqs) != 1 || reqs[0].body["type"] != float64(discordgo.InteractionResponseModal) {
		t.Fatalf("form route was deferred or not answered: %+v", reqs)
	}
}

func TestRouterMatching(t *testing.T) {
	router, _, _ := testRouter(t)
	nop := func(*discordgo.Session, *discordgo.InteractionCreate) {}
	router.Command("server", AckPrivate, nop, SubAck{Path: "connect", Ack: AckSelf})
	router.ComponentPrefix("shop:", AckUpdate, nop)
	router.ComponentPrefix("shop:issue:", AckSelf, nop)
	router.Component("exact", AckPublic, nop)
	router.ModalPrefix("shop:", AckUpdate, nop)
	want := []struct {
		kind discordgo.InteractionType
		name string
		ack  AckMode
	}{
		{discordgo.InteractionApplicationCommand, "server connect", AckSelf},
		{discordgo.InteractionApplicationCommand, "server status", AckPrivate},
		{discordgo.InteractionApplicationCommand, "server", AckPrivate},
		{discordgo.InteractionMessageComponent, "shop:received:7", AckUpdate},
		{discordgo.InteractionMessageComponent, "shop:issue:7", AckSelf},
		{discordgo.InteractionMessageComponent, "exact", AckPublic},
		{discordgo.InteractionModalSubmit, "shop:issuemodal:7", AckUpdate},
	}
	for _, w := range want {
		rt, ack := router.find(newTestInteraction(t, w.kind, w.name))
		if rt == nil || ack != w.ack {
			t.Errorf("%s: route=%v ack=%v, want %v", w.name, rt != nil, ack, w.ack)
		}
	}
	for _, miss := range []struct {
		kind discordgo.InteractionType
		name string
	}{{discordgo.InteractionApplicationCommand, "other"}, {discordgo.InteractionMessageComponent, "exactly"}, {discordgo.InteractionModalSubmit, "exact"}, {discordgo.InteractionApplicationCommandAutocomplete, "server"}} {
		if rt, _ := router.find(newTestInteraction(t, miss.kind, miss.name)); rt != nil {
			t.Errorf("%s matched a route of another kind or name", miss.name)
		}
	}
	if got := len(router.Routes()); got != 5 {
		t.Fatalf("Routes() = %d entries", got)
	}
}

// A route cannot be registered without saying how it is acknowledged.
func TestRouteRegistrationRequiresAnAckMode(t *testing.T) {
	nop := func(*discordgo.Session, *discordgo.InteractionCreate) {}
	mustPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: registration was accepted", name)
			}
		}()
		fn()
	}
	router, _, _ := testRouter(t)
	mustPanic("no mode", func() { router.Command("a", 0, nop) })
	mustPanic("unknown mode", func() { router.Component("a", AckMode(99), nop) })
	mustPanic("no handler", func() { router.Command("a", AckPrivate, nil) })
	mustPanic("subcommand without mode", func() { router.Command("a", AckPrivate, nop, SubAck{Path: "x"}) })
	router.Command("a", AckPrivate, nop)
	mustPanic("duplicate", func() { router.Command("a", AckPrivate, nop) })
}

// The routes handlers register for themselves.
func TestPanelAndRentRoutes(t *testing.T) {
	router, _, _ := testRouter(t)
	(&PublicPanelHandler{}).Register(router)
	(&BaseCommandHandler{}).RegisterRent(router)
	want := map[string]AckMode{
		linkPanelOpenID: AckSelf, statsPanelSearchID: AckSelf, // open a form
		statsPanelMeID: AckPrivate, economyBalanceID: AckPrivate, economyHistoryID: AckPrivate,
		rentAskPrefix + "4": AckPrivate, rentPayPrefix + "4": AckUpdate, rentCancelID: AckUpdate, "baserent:unknown": AckPrivate,
	}
	for id, ack := range want {
		rt, got := router.find(newTestInteraction(t, discordgo.InteractionMessageComponent, id))
		if rt == nil || got != ack {
			t.Errorf("button %s: routed=%v ack=%v, want %v", id, rt != nil, got, ack)
		}
	}
	for _, id := range []string{linkPanelModalID, statsSearchModalID} {
		if rt, got := router.find(newTestInteraction(t, discordgo.InteractionModalSubmit, id)); rt == nil || got != AckPrivate {
			t.Errorf("form %s is not routed privately", id)
		}
	}
}

// Every button the public panels post must have a route: the economy buttons
// were posted for months without one and always failed.
func TestEveryPublicPanelButtonIsRouted(t *testing.T) {
	router, _, _ := testRouter(t)
	(&PublicPanelHandler{}).Register(router)
	var ids []string
	var walk func(components []discordgo.MessageComponent)
	walk = func(components []discordgo.MessageComponent) {
		for _, c := range components {
			switch v := c.(type) {
			case discordgo.ActionsRow:
				walk(v.Components)
			case *discordgo.ActionsRow:
				walk(v.Components)
			case discordgo.Button:
				ids = append(ids, v.CustomID)
			}
		}
	}
	walk(LinkUsernamePanelComponents())
	walk(PlayerStatsPanelComponents())
	if len(ids) < 5 {
		t.Fatalf("panel buttons not found: %v", ids)
	}
	for _, id := range ids {
		if rt, _ := router.find(newTestInteraction(t, discordgo.InteractionMessageComponent, id)); rt == nil {
			t.Errorf("panel button %s has no route", id)
		}
	}
}

type slowGuildStore struct{ release chan struct{} }

func (g slowGuildStore) UpsertGuild(context.Context, repository.GuildRecord) (int64, error) {
	return 0, nil
}

func (g slowGuildStore) GetGuild(ctx context.Context, _ string) (*repository.GuildRecord, int64, error) {
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	return nil, 0, context.Canceled
}

// Autocomplete cannot be deferred, so a slow service lookup is answered with
// an empty list inside the window instead of timing out.
func TestServerAutocompleteAnswersWithinItsBudget(t *testing.T) {
	store := slowGuildStore{release: make(chan struct{})}
	defer close(store.release)
	h := &ServerCommandHandler{servers: &repository.ServerRepository{}, guilds: store}
	started := time.Now()
	if _, err := h.suggestedServicesWithin("g", 20*time.Millisecond); err == nil {
		t.Fatal("slow lookup returned services")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("waited %s for a 20 ms budget", elapsed)
	}
	if autocompleteBudget >= 3*time.Second-ackAfter {
		t.Fatalf("autocomplete budget %s leaves no room inside Discord's 3 s", autocompleteBudget)
	}
}

func TestIsAdminUsesInteractionPermissionsWithoutAskingDiscord(t *testing.T) {
	s, rec := recordingSession(t)
	member := func(perms int64) *discordgo.InteractionCreate {
		return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{GuildID: "g", ChannelID: "c", Member: &discordgo.Member{User: &discordgo.User{ID: "u"}, Permissions: perms}}}
	}
	if !isAdmin(s, member(discordgo.PermissionManageServer)) || isAdmin(s, member(discordgo.PermissionSendMessages)) {
		t.Fatal("permission check is wrong")
	}
	if reqs := rec.snapshot(); len(reqs) != 0 {
		t.Fatalf("permission check asked Discord before answering: %+v", reqs)
	}
}

// --- structural guard ---------------------------------------------------------------------------

// sourceFiles returns the non-test Go files of a package directory.
func sourceFiles(t *testing.T, dir string) []string {
	t.Helper()
	all, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(all) == 0 {
		t.Fatalf("no Go files in %s: %v", dir, err)
	}
	var out []string
	for _, name := range all {
		if !strings.HasSuffix(name, "_test.go") {
			out = append(out, name)
		}
	}
	return out
}

// Every acknowledgement goes through interaction_defer.go and every handler is
// registered on the router. This fails when new code answers an interaction
// directly, edits an interaction reply directly, or listens for interactions
// on the session itself, because any of those bypasses the guarantee that an
// interaction is acknowledged in time.
func TestInteractionsOnlyGoThroughTheAcknowledgingPath(t *testing.T) {
	// method -> the one file allowed to call it
	guarded := map[string]string{
		"InteractionRespond":        "interaction_defer.go",
		"InteractionResponseEdit":   "interaction_defer.go",
		"InteractionResponseDelete": "interaction_defer.go",
		"FollowupMessageCreate":     "interaction_defer.go",
	}
	fset := token.NewFileSet()
	checked := 0
	for _, dir := range []string{".", "../app", "panels"} {
		for _, name := range sourceFiles(t, dir) {
			file, err := parser.ParseFile(fset, name, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			checked++
			base := filepath.Base(name)
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if allowed, isGuarded := guarded[sel.Sel.Name]; isGuarded && !(dir == "." && base == allowed) {
					t.Errorf("%s: %s is called directly; use the reply helpers in internal/discord/%s so the interaction is acknowledged in time",
						fset.Position(call.Pos()), sel.Sel.Name, allowed)
				}
				if sel.Sel.Name == "AddHandler" && !(dir == "." && base == "client.go") {
					t.Errorf("%s: AddHandler is called outside internal/discord/client.go; register interactions on Client.Interactions()", fset.Position(call.Pos()))
				}
				return true
			})
			// In client.go the only interaction listener is the router.
			if dir == "." && base == "client.go" {
				ast.Inspect(file, func(n ast.Node) bool {
					lit, ok := n.(*ast.FuncLit)
					if !ok {
						return true
					}
					for _, param := range lit.Type.Params.List {
						if star, ok := param.Type.(*ast.StarExpr); ok {
							if sel, ok := star.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "InteractionCreate" {
								t.Errorf("%s: an interaction listener is registered outside the router", fset.Position(lit.Pos()))
							}
						}
					}
					return true
				})
			}
		}
	}
	if checked < 50 {
		t.Fatalf("only %d files scanned; the guard is not looking at the source", checked)
	}
	if _, err := os.Stat("interaction_defer.go"); err != nil {
		t.Fatal(err)
	}
}
