package repository

import "testing"

func TestDeleteConfirmationMatches(t *testing.T) {
	for _, tc := range []struct {
		expected, typed string
		want            bool
	}{
		{"Wasteland Kings", "Wasteland Kings", true},
		{"Wasteland Kings", "  Wasteland Kings\n", true},
		{"Wasteland Kings", "wasteland kings", false},
		{"Wasteland Kings", "Wasteland  Kings", false},
		{"Wasteland Kings", "Wasteland", false},
		{"Wasteland Kings", "", false},
		{"42", "42", true},
		{"42", "042", false},
		{"42", "421", false},
		{"", "", false},
		{"   ", "", false},
	} {
		if got := DeleteConfirmationMatches(tc.expected, tc.typed); got != tc.want {
			t.Errorf("DeleteConfirmationMatches(%q, %q) = %v, want %v", tc.expected, tc.typed, got, tc.want)
		}
	}
}

func TestSubscriptionIsLivePaid(t *testing.T) {
	for _, tc := range []struct {
		providerID, status string
		want               bool
	}{
		{"sub_1", SubscriptionActive, true},
		{"sub_1", SubscriptionPastDue, true},
		{"sub_1", SubscriptionTrial, true},
		{"sub_1", SubscriptionSuspended, true},
		{"sub_1", SubscriptionCanceled, false},
		{"sub_1", SubscriptionInactive, false},
		{"", SubscriptionActive, false}, // an owner grant: active, but nobody is paying
		{"", SubscriptionTrial, false},
		{"  ", SubscriptionActive, false},
	} {
		if got := SubscriptionIsLivePaid(tc.providerID, tc.status); got != tc.want {
			t.Errorf("SubscriptionIsLivePaid(%q, %q) = %v, want %v", tc.providerID, tc.status, got, tc.want)
		}
	}
}

func TestRecordBlockersSeparatesConfigurationFromRecords(t *testing.T) {
	removed, blockers := recordBlockers(map[string]int64{
		"installation_settings": 1, "installation_channel_routes": 4, "organization_members": 2,
		"shop_purchases": 3, "hub_factions": 1, "a_table_added_later": 5, "installation_zones": 0,
	})
	if len(removed) != 3 || removed["installation_channel_routes"] != 4 || removed["organization_members"] != 2 {
		t.Fatalf("removed: %v", removed)
	}
	if len(blockers) != 3 {
		t.Fatalf("blockers: %v", blockers)
	}
	// Sorted by table, and a table nobody classified blocks.
	if blockers[0].Code != DeleteBlockerRecords || blockers[0].Message != "It has 5 record(s) that would be lost (a_table_added_later)." {
		t.Fatalf("unknown table: %+v", blockers[0])
	}
	if blockers[1].Message != "It has 1 faction record(s) that would be lost (hub_factions)." || blockers[2].Message != "It has 3 shop order record(s) that would be lost (shop_purchases)." {
		t.Fatalf("labels: %+v", blockers)
	}
	if removed, blockers := recordBlockers(nil); len(removed) != 0 || len(blockers) != 0 {
		t.Fatal("nothing below the row must mean nothing removed and nothing blocking")
	}
}

func TestOnlyConfigurationTablesAreDeletable(t *testing.T) {
	for _, table := range []string{"billing_transactions", "shop_purchases", "shop_deliveries", "hub_factions", "case_review_cases",
		"case_addon_subscriptions", "kills", "deaths", "players", "player_links", "game_servers", "guilds", "app_users", "organizations"} {
		if IsDeleteConfigTable(table) {
			t.Errorf("%s must never be treated as configuration", table)
		}
	}
	for _, table := range []string{"organization_members", "installations", "discord_guild_connections", "installation_setup_progress", "installation_channel_routes", "subscriptions"} {
		if !IsDeleteConfigTable(table) {
			t.Errorf("%s is configuration", table)
		}
	}
}
