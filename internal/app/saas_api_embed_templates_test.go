package app

import (
	"context"
	"sort"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/embedrender"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
)

// The template registry matches text-channel route vocabulary. Some routes,
// including C.A.S.E., can be designed before a real publisher is reviewed.
func TestEmbedTemplateRoutesMatchTheChannelRouteBlueprint(t *testing.T) {
	var blueprint, templ []string
	for k := range championRouteKeys {
		if k == "ONLINE_COUNTER" {
			// A voice counter is not a message route. C.A.S.E. routes
			// support stored/previewed templates but no live publisher yet.
			continue
		}
		blueprint = append(blueprint, k)
	}
	templ = embedtemplates.RouteKeys()
	sort.Strings(blueprint)
	sort.Strings(templ)
	if len(blueprint) != len(templ) {
		t.Fatalf("blueprint %v vs templates %v", blueprint, templ)
	}
	for i := range blueprint {
		if blueprint[i] != templ[i] {
			t.Fatalf("blueprint %v vs templates %v", blueprint, templ)
		}
	}
	for _, route := range []string{"CASE_STATUS", "CASE_EVIDENCE", "CASE_ALERTS"} {
		if !embedtemplates.ValidRoute(route) {t.Fatalf("%s missing template contract", route)}
		if (&App{}).runtimeRenderingFor(0, route) != "NOT_ENABLED" {
			t.Fatalf("%s has no authorized live publisher", route)
		}
	}
	// Every route the website's designer offers (lib/saas/embedTypes.ts) is accepted.
	for _, k := range []string{"KILLFEED", "PVE_FEED", "HITFEED", "BOUNTY", "BOUNTY_TRACKING", "CONNECTIONS", "ECONOMY", "SHOP", "BUILD_FEED", "ADMIN_ALERTS", "ADMIN_LOGS"} {
		if !embedtemplates.ValidRoute(k) {
			t.Errorf("%s must be a valid template route", k)
		}
	}
}

// The API never claims a template is live unless it will actually render: the flag must
// be on AND the route must be one the runtime publishers render.
func TestRuntimeRenderingStatusReflectsFlagAndRouteSupport(t *testing.T) {
	off := &App{}
	on := &App{EmbedRenderer: embedrender.New(embedrender.Options{Source: nopSource{}, Enabled: true})}
	disabled := &App{EmbedRenderer: embedrender.New(embedrender.Options{Source: nopSource{}, Enabled: false})}
	for _, route := range embedrender.SupportedRoutes() {
		if on.runtimeRenderingFor(0, route) != "ENABLED" {
			t.Errorf("%s must report ENABLED when the flag is on", route)
		}
		if off.runtimeRenderingFor(0, route) != "NOT_ENABLED" || disabled.runtimeRenderingFor(0, route) != "NOT_ENABLED" {
			t.Errorf("%s must report NOT_ENABLED when the flag is off", route)
		}
	}
	// Persistent boards, diagnostic monitors and routes without a publisher stay NOT_ENABLED even with the flag on.
	for _, route := range []string{"BOUNTY", "ADMIN_LOGS", "ADMIN_ALERTS", "BUILD_FEED", "SHOP", "HEATMAPS", "LINK_GAMERTAG", "STATS_LEADERBOARDS", "AUTO_LEADERBOARD"} {
		if on.runtimeRenderingFor(0, route) != "NOT_ENABLED" {
			t.Errorf("%s has no runtime template rendering and must say so", route)
		}
	}
	// Every supported route is a valid template route.
	for _, route := range embedrender.SupportedRoutes() {
		if !embedtemplates.ValidRoute(route) {
			t.Errorf("%s supported but not a template route", route)
		}
	}
}

type nopSource struct{}

func (nopSource) ResolveTemplate(context.Context, int64, int64, string) (int64, *embedtemplates.Config, error) {
	return 0, nil, nil
}
