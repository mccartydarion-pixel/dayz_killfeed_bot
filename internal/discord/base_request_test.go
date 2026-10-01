package discord

import (
	"strings"
	"testing"
)

func TestBaseRequestDecisionMessage(t *testing.T) {
	ok := BaseRequestDecisionMessage(true, "Hill@top", "Champions", "")
	if e := ok.Embeds[0]; !strings.Contains(e.Title, "is registered") || len(e.Fields) != 0 || len(ok.AllowedMentions.Parse) != 0 {
		t.Fatalf("approved: %+v", e)
	}
	no := BaseRequestDecisionMessage(false, "Shack", "Champions", "Inside the safe zone @everyone")
	e := no.Embeds[0]
	if !strings.Contains(e.Title, "wasn't approved") || len(e.Fields) != 1 || strings.Contains(e.Fields[0].Value, "@everyone") {
		t.Fatalf("declined: %+v %+v", e, e.Fields)
	}
	if blank := BaseRequestDecisionMessage(false, "", "", "").Embeds[0]; len(blank.Fields) != 0 || !strings.Contains(blank.Description, "your server") {
		t.Fatalf("blank: %+v", blank)
	}
}
