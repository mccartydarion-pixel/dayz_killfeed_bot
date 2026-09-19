package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/adminrepo"
	"github.com/yourname/dayz-killfeed/internal/config"
)

// The Founder Hub website (Champions_Killfed_Website, lib/admin/types.ts) consumes
// these exact shapes. Kinds: num, str, str? (string|null), bool?, arr, obj, obj?.
// Extra keys are allowed (the API adds authoritative detail); missing keys or a
// wrong kind would break a page, so they fail here.
type shape map[string]string

var (
	shapeSubscription = shape{"plan": "str", "status": "str", "trialEndsAt": "str?", "entitlements": "arr", "createdAt": "str?", "updatedAt": "str?"}
	shapeInstallation = shape{"id": "num", "discordGuild": "str?", "dayzServer": "str?", "platform": "str?", "plan": "str", "status": "str", "health": "str",
		"currentSetupStep": "str?", "setupCompletedAt": "str?", "lastHealthCheckAt": "str?"}
	shapeRoute        = shape{"routeKey": "str", "channelName": "str?", "channelId": "str?"}
	shapeSettings     = shape{"timezone": "str", "distanceUnit": "str", "onlineDisplayEnabled": "bool", "leaderboardEnabled": "bool"}
	shapeMember       = shape{"id": "str", "displayName": "str", "discordId": "str", "role": "str"}
	shapeOrganization = shape{"id": "num", "name": "str", "slug": "str", "createdAt": "str?", "owner": "str?", "memberCount": "num",
		"discordGuild": "str?", "dayzServer": "str?", "subscription": "obj?", "installations": "arr"}
	shapeOverview = shape{"totalOrganizations": "num", "totalUsers": "num", "activeInstallations": "num", "readyInstallations": "num",
		"configuringInstallations": "num", "degradedInstallations": "num", "trials": "num", "activeSubscriptions": "num",
		"suspendedSubscriptions": "num", "backendStatus": "str"}
	shapeHealthItem = shape{"id": "num", "organization": "str", "health": "str", "discordBotInstalled": "bool?", "serverStatus": "str?", "lastHealthCheckAt": "str?"}
)

func kindOf(v any, present bool) string {
	if !present {
		return "missing"
	}
	switch v.(type) {
	case nil:
		return "null"
	case float64:
		return "num"
	case string:
		return "str"
	case bool:
		return "bool"
	case []any:
		return "arr"
	case map[string]any:
		return "obj"
	default:
		return "other"
	}
}

// checkShape verifies every key of want exists with a compatible kind.
func checkShape(t *testing.T, label string, got any, want shape) {
	t.Helper()
	obj, ok := got.(map[string]any)
	if !ok {
		t.Errorf("%s: expected an object, got %T", label, got)
		return
	}
	for key, kind := range want {
		v, present := obj[key]
		k := kindOf(v, present)
		base, isNullable := kind, false
		if n := len(kind); n > 0 && kind[n-1] == '?' {
			base, isNullable = kind[:n-1], true
		}
		if k == "missing" || (k == "null" && !isNullable) || (k != "null" && k != base) {
			t.Errorf("%s.%s: expected %s, got %s (%v)", label, key, kind, k, v)
		}
	}
}

// richContractReader returns fully populated rows, so every website-required field
// is checked with a real (non-null) value as well as the nullable ones.
type richContractReader struct{ fakeAdminReader }

func richInstallation(id int64) adminrepo.InstallationSummary {
	guild, server, platform, step, done, check := "Guild", "Server", "PLAYSTATION", "CHANNELS", "2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z"
	return adminrepo.InstallationSummary{ID: id, InstallationID: id, OrganizationID: 7, Organization: "Org", DiscordGuild: &guild, DayZServer: &server, Platform: &platform,
		Plan: "TRIAL", Status: "READY", Health: "HEALTHY", CurrentSetupStep: &step, SetupCompletedAt: &done, LastHealthCheckAt: &check, CreatedAt: "2026-01-01T00:00:00Z",
		Discord: adminrepo.DiscordInfo{GuildID: "1", GuildName: guild, BotInstalled: true}, Server: &adminrepo.ServerInfo{ID: 4, DisplayName: server, Platform: platform}}
}

func richSub() *adminrepo.SubscriptionInfo {
	t := "2026-02-01T00:00:00Z"
	return &adminrepo.SubscriptionInfo{Plan: "TRIAL", Status: "TRIAL", TrialEndsAt: &t, Entitlements: []string{"killfeed"}, CreatedAt: &t, UpdatedAt: &t}
}

func richOrg(id int64) adminrepo.Organization {
	created, owner, guild, server := "2026-01-01T00:00:00Z", "Alice", "Guild", "Server"
	return adminrepo.Organization{ID: id, Name: "Org", Slug: "org", CreatedAt: &created, Owner: &owner, OwnerUser: &adminrepo.UserRef{UserID: 1, DisplayName: owner, DiscordID: "1"},
		MemberCount: 2, InstallationCount: 1, DiscordGuild: &guild, DayZServer: &server, Subscription: richSub(), Installations: []adminrepo.InstallationSummary{richInstallation(9)},
		Members: []adminrepo.MemberRow{{ID: "1", DisplayName: owner, DiscordID: "1", Role: "OWNER"}}}
}

func (r *richContractReader) ListOrganizations(context.Context, adminrepo.OrganizationFilter) ([]adminrepo.Organization, int64, error) {
	return []adminrepo.Organization{richOrg(7)}, 7, nil
}
func (r *richContractReader) GetOrganization(context.Context, int64) (*adminrepo.Organization, error) {
	o := richOrg(7)
	return &o, nil
}
func (r *richContractReader) ListSubscriptions(context.Context, adminrepo.SubscriptionFilter) ([]adminrepo.SubscriptionRow, int64, error) {
	t := "2026-02-01T00:00:00Z"
	return []adminrepo.SubscriptionRow{{ID: 5, Organization: "Org", OrganizationID: 7, OrganizationName: "Org", Plan: "TRIAL", Status: "TRIAL", TrialEndsAt: &t,
		Entitlements: []string{"killfeed"}, InstallationCount: 1, CreatedAt: &t, UpdatedAt: &t}}, 5, nil
}
func (r *richContractReader) ListInstallations(context.Context, adminrepo.InstallationFilter) ([]adminrepo.InstallationSummary, int64, error) {
	return []adminrepo.InstallationSummary{richInstallation(9)}, 9, nil
}
func (r *richContractReader) GetInstallation(context.Context, int64) (*adminrepo.InstallationDetail, error) {
	gs := &adminrepo.GeneralSettings{Timezone: "UTC", DistanceUnit: "METERS", OnlineDisplayEnabled: true, LeaderboardEnabled: true}
	return &adminrepo.InstallationDetail{InstallationSummary: richInstallation(9), OrganizationSlug: "org", Subscription: richSub(), Settings: gs, GeneralSettings: gs,
		ChannelRoutes: []adminrepo.ChannelRoute{{RouteKey: "KILLFEED", ChannelID: "c-1"}}}, nil
}
func (r *richContractReader) InstallationHealth(context.Context) (adminrepo.HealthSummary, []adminrepo.HealthInstallation, error) {
	bot, st, check := true, "ACTIVE", "2026-01-02T00:00:00Z"
	return adminrepo.HealthSummary{ByStatus: map[string]int{}, ByHealth: map[string]int{}, ServersByStatus: map[string]int{}},
		[]adminrepo.HealthInstallation{{ID: 9, Organization: "Org", Health: "DEGRADED", DiscordBotInstalled: &bot, ServerStatus: &st, LastHealthCheckAt: &check}}, nil
}

func decodeJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, body)
	}
	return m
}

func first(t *testing.T, m map[string]any, key string) any {
	t.Helper()
	arr, ok := m[key].([]any)
	if !ok || len(arr) == 0 {
		t.Fatalf("expected a non-empty %s array in %v", key, m)
	}
	return arr[0]
}

func TestAdminAPIMatchesTheFounderHubWebsiteContract(t *testing.T) {
	fake := &richContractReader{}
	a := &App{Config: &config.Config{WebsiteAPISecret: adminTestSecret, AdminDiscordIDs: []string{adminTestAdmin}}, adminSaaS: fake}
	get := func(h adminHandler, pv map[string]string) map[string]any {
		rr := adminGet(a, h, "/x", adminTestSecret, adminTestAdmin, pv)
		if rr.Code != http.StatusOK {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
		return decodeJSON(t, rr.Body.Bytes())
	}

	checkShape(t, "overview", get(a.handleAdminOverview, nil), shapeOverview)

	orgs := get(a.handleAdminListOrganizations, nil)
	checkShape(t, "organizations page", orgs, shape{"items": "arr", "nextCursor": "str?"})
	org := first(t, orgs, "items")
	checkShape(t, "organizations[0]", org, shapeOrganization)
	checkShape(t, "organizations[0].subscription", org.(map[string]any)["subscription"], shapeSubscription)
	checkShape(t, "organizations[0].installations[0]", first(t, org.(map[string]any), "installations"), shapeInstallation)

	detail := get(a.handleAdminGetOrganization, map[string]string{"organizationID": "7"})
	checkShape(t, "organization detail", detail, shapeOrganization)
	checkShape(t, "organization detail.members[0]", first(t, detail, "members"), shapeMember)

	subs := get(a.handleAdminListSubscriptions, nil)
	checkShape(t, "subscriptions page", subs, shape{"items": "arr", "nextCursor": "str?"})
	sub := shape{"organization": "str", "installationCount": "num"}
	for k, v := range shapeSubscription {
		sub[k] = v
	}
	checkShape(t, "subscriptions[0]", first(t, subs, "items"), sub)

	insts := get(a.handleAdminListInstallations, nil)
	checkShape(t, "installations page", insts, shape{"items": "arr", "nextCursor": "str?"})
	listItem := shape{"organization": "str"}
	for k, v := range shapeInstallation {
		listItem[k] = v
	}
	checkShape(t, "installations[0]", first(t, insts, "items"), listItem)

	inst := get(a.handleAdminGetInstallation, map[string]string{"installationID": "9"})
	detailShape := shape{"organization": "str", "subscription": "obj?", "channelRoutes": "arr", "settings": "obj?"}
	for k, v := range shapeInstallation {
		detailShape[k] = v
	}
	checkShape(t, "installation detail", inst, detailShape)
	checkShape(t, "installation detail.subscription", inst["subscription"], shapeSubscription)
	checkShape(t, "installation detail.settings", inst["settings"], shapeSettings)
	checkShape(t, "installation detail.generalSettings", inst["generalSettings"], shapeSettings)
	checkShape(t, "installation detail.channelRoutes[0]", first(t, inst, "channelRoutes"), shapeRoute)

	h := get(a.handleAdminHealth, nil)
	checkShape(t, "health", h, shape{"backendStatus": "str", "installations": "arr"})
	checkShape(t, "health.installations[0]", first(t, h, "installations"), shapeHealthItem)
}

// The nested/authoritative additions the API layers on top of the website shape.
func TestAdminAPIExposesAuthoritativeDetailBesideTheWebsiteShape(t *testing.T) {
	a := &App{Config: &config.Config{WebsiteAPISecret: adminTestSecret, AdminDiscordIDs: []string{adminTestAdmin}}, adminSaaS: &richContractReader{}}
	rr := adminGet(a, a.handleAdminListInstallations, "/x", adminTestSecret, adminTestAdmin, nil)
	item := first(t, decodeJSON(t, rr.Body.Bytes()), "items").(map[string]any)
	checkShape(t, "discord", item["discord"], shape{"guildId": "str", "guildName": "str", "botInstalled": "bool"})
	checkShape(t, "server", item["server"], shape{"id": "num", "serviceId": "str", "displayName": "str", "platform": "str", "status": "str"})
	if item["installationId"] == nil || item["organizationId"] == nil {
		t.Fatalf("installationId and organizationId must be present: %v", item)
	}
	rr = adminGet(a, a.handleAdminListOrganizations, "/x", adminTestSecret, adminTestAdmin, nil)
	org := first(t, decodeJSON(t, rr.Body.Bytes()), "items").(map[string]any)
	checkShape(t, "ownerUser", org["ownerUser"], shape{"id": "num", "displayName": "str", "discordId": "str"})
	if org["installationCount"] == nil {
		t.Fatal("installationCount must be present")
	}
}
