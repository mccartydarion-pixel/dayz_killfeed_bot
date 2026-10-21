package staffactivity

import "testing"

func TestOf(t *testing.T) {
	for action, want := range map[string]string{
		"WARNING_ISSUED": "MODERATION", "CASE_REVIEW_QUEUE_VIEWED": "VIEWS", "ZONE_BAN_ADD": "MODERATION", "ZONE_CREATE": "ZONES",
		"PERMISSION_CHANGE": "ACCESS", "SERVER_RESTART": "SERVER", "REWARD_RULE_SAVE": "ECONOMY", "SEASON_CHANGE_SCHEDULE": "COMMUNITY",
		"SECURITY_GIFT_GIVEN": "ECONOMY", "SECURITY_PANEL_SAVED": "ZONES", "SOMETHING_NEW": Other,
	} {
		if got := Of(action); got != want {
			t.Errorf("%s: got %s want %s", action, got, want)
		}
	}
}
