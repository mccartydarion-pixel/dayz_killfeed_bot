package embedtemplates

import (
	"encoding/json"
	"strings"
	"testing"
)

// The Champion website's Phase 1 designer (lib/saas/embedTypes.ts) ships a default
// template and an approved-variable list for each of its routes. Every one of those
// defaults must be accepted unchanged by the backend, and every variable the website
// offers must be approved here - otherwise a customer could not save what the designer
// shows them. (Copied from the website's `definitions`; update together. CASINO was
// removed from Champion entirely, so the website must drop it too.)
type websiteRoute struct {
	key       string
	variables []string
	color     string
	title     string
	desc      string
	fields    [][3]string // key, label, template
	inline    bool
}

var websiteRoutes = []websiteRoute{
	{"KILLFEED", []string{"killer", "victim", "weapon", "distance", "server_name", "timestamp"}, "#D4AF37", "{{killer}} eliminated {{victim}}", "A {{weapon}} kill from {{distance}} away.",
		[][3]string{{"killer", "Killer", "{{killer}}"}, {"victim", "Victim", "{{victim}}"}, {"weapon", "Weapon", "{{weapon}}"}, {"distance", "Distance", "{{distance}}"}}, true},
	{"PVE_FEED", []string{"victim", "server_name", "timestamp"}, "#8C6A2D", "PvE event", "{{victim}} was lost to the wilds.", [][3]string{{"victim", "Player", "{{victim}}"}}, false},
	{"HITFEED", []string{"killer", "victim", "weapon", "distance"}, "#C98B3D", "Hit confirmed", "{{killer}} hit {{victim}} with {{weapon}}.", [][3]string{{"distance", "Distance", "{{distance}}"}}, true},
	{"BOUNTY", []string{"victim", "server_name", "timestamp"}, "#B84C4C", "Bounty posted", "A new bounty is active for {{victim}}.", [][3]string{{"target", "Target", "{{victim}}"}}, false},
	{"BOUNTY_TRACKING", []string{"killer", "victim", "server_name"}, "#9E4B4B", "Bounty update", "{{killer}} is tracking {{victim}}.", [][3]string{{"hunter", "Hunter", "{{killer}}"}, {"target", "Target", "{{victim}}"}}, true},
	{"ECONOMY", []string{"player", "amount", "balance", "server_name"}, "#4C9A72", "Economy transaction", "{{player}} received {{amount}} credits.", [][3]string{{"amount", "Amount", "{{amount}}"}, {"balance", "Balance", "{{balance}}"}}, true},
	{"SHOP", []string{"player", "item", "amount", "balance"}, "#4C7FA0", "Shop purchase", "{{player}} purchased {{item}}.", [][3]string{{"item", "Item", "{{item}}"}, {"amount", "Cost", "{{amount}}"}, {"balance", "Balance", "{{balance}}"}}, false},
	{"CONNECTIONS", []string{"player", "event", "server_name", "timestamp"}, "#4C87A0", "Player connection", "{{player}} {{event}} the server.", [][3]string{{"server", "Server", "{{server_name}}"}}, false},
	{"BUILD_FEED", []string{"player", "structure", "server_name"}, "#8A6F4A", "Build event", "{{player}} placed {{structure}}.", [][3]string{{"structure", "Structure", "{{structure}}"}}, false},
	{"ADMIN_ALERTS", []string{"event", "player", "server_name", "timestamp"}, "#D17A3F", "Admin alert", "{{event}} involving {{player}}.", [][3]string{{"event", "Event", "{{event}}"}, {"player", "Player", "{{player}}"}}, false},
	{"ADMIN_LOGS", []string{"event", "player", "server_name", "timestamp"}, "#697386", "Admin log", "{{event}} by {{player}}.", [][3]string{{"event", "Event", "{{event}}"}, {"player", "Actor", "{{player}}"}}, false},
}

// websiteJSON builds the exact wire shape lib/saas/embedTypes.ts serializes (EmbedTemplate).
func websiteJSON(r websiteRoute) []byte {
	fields := []map[string]any{}
	for i, f := range r.fields {
		fields = append(fields, map[string]any{"key": f[0], "label": f[1], "enabled": true, "template": f[2], "inline": r.inline, "order": i})
	}
	b, _ := json.Marshal(map[string]any{
		"routeKey": r.key, "enabled": true, "color": r.color,
		"title": map[string]any{"enabled": true, "template": r.title}, "description": map[string]any{"enabled": true, "template": r.desc},
		"author":    map[string]any{"enabled": false, "name": "Champion", "iconUrl": ""},
		"thumbnail": map[string]any{"enabled": false, "url": ""}, "image": map[string]any{"enabled": false, "url": ""},
		"footer": map[string]any{"enabled": true, "text": "Champion Killfeed", "iconUrl": ""}, "timestamp": true, "fields": fields,
	})
	return b
}

func TestEveryWebsiteDefaultTemplateIsAcceptedUnchanged(t *testing.T) {
	if len(websiteRoutes) != 11 {
		t.Fatalf("the website offers 11 routes, table has %d", len(websiteRoutes))
	}
	for _, r := range websiteRoutes {
		var cfg Config
		dec := json.NewDecoder(strings.NewReader(string(websiteJSON(r))))
		dec.DisallowUnknownFields() // the strict decoder the API uses
		if err := dec.Decode(&cfg); err != nil {
			t.Fatalf("%s: the website's JSON must decode into the typed model: %v", r.key, err)
		}
		out, err := Validate(cfg, r.key)
		if err != nil {
			t.Errorf("%s: the website's own default must validate: %v", r.key, err)
			continue
		}
		if out.Color != r.color || out.Title.Template != r.title || out.Description.Template != r.desc || len(out.Fields) != len(r.fields) {
			t.Errorf("%s: a valid default must be stored as written: %+v", r.key, out)
		}
		// Every variable the website's designer offers for the route is approved here.
		approved := map[string]bool{}
		for _, v := range Variables(r.key) {
			approved[v] = true
		}
		for _, v := range r.variables {
			if !approved[v] {
				t.Errorf("%s: the designer offers {{%s}} but the backend does not approve it", r.key, v)
			}
		}
	}
}
