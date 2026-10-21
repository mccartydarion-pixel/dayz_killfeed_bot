package discord

import (
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/economy"
)

func TestRewardCardSaysWhy(t *testing.T) {
	e := economy.Event{Type: economy.TypeSystemReward, PlayerName: "Dreybert", Amount: 10000, Credit: true, BalanceAfter: 10000, Reason: "Daily play reward (3 days in a row)"}
	got := buildEconomyEmbed(e).Description
	if !strings.Contains(got, "Dreybert earned 10,000 pts\nFor: Daily play reward (3 days in a row)\nBalance: 10,000 pts") {
		t.Fatalf("reward card: %q", got)
	}
	e.Reason = ""
	if got := buildEconomyEmbed(e).Description; strings.Contains(got, "For:") {
		t.Fatalf("no reason, no line: %q", got)
	}
	if got := rewardReason("Hi @everyone <@123> [x](http://evil) **bold**"); strings.Contains(got, "@everyone") || strings.Contains(got, "<@") || strings.Contains(got, "](") || strings.Contains(got, "**") {
		t.Fatalf("reason is made safe: %q", got)
	}
	if got := rewardReason(strings.Repeat("a", 300)); len([]rune(got)) != 120 {
		t.Fatalf("reason is capped: %d", len([]rune(got)))
	}
	if economyVars(economy.Event{Type: economy.TypeSystemReward, Reason: "Challenge: Get 3 kills"}, "")["reason"] != "Challenge: Get 3 kills" {
		t.Fatal("custom cards get {{reason}}")
	}
}
