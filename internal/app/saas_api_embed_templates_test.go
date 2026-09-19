package app

import (
	"sort"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
)

// The route set a template can be stored for must be EXACTLY the channel-route
// blueprint (single source of truth for route keys): a route added there without a
// variable set here (or the reverse) fails this test.
func TestEmbedTemplateRoutesMatchTheChannelRouteBlueprint(t *testing.T) {
	var blueprint, templ []string
	for k := range championRouteKeys {
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
	// Every route the website's designer offers (lib/saas/embedTypes.ts) is accepted.
	for _, k := range []string{"KILLFEED", "PVE_FEED", "HITFEED", "BOUNTY", "BOUNTY_TRACKING", "CONNECTIONS", "ECONOMY", "CASINO", "SHOP", "BUILD_FEED", "ADMIN_ALERTS", "ADMIN_LOGS"} {
		if !embedtemplates.ValidRoute(k) {
			t.Errorf("%s must be a valid template route", k)
		}
	}
}
