package discord

import (
	"strings"
	"testing"
	"time"
)

// Phase 6.26F.2: the operational ADMIN_ALERTS publisher never carries paid C.A.S.E. Watch
// evidence, even when an ADMIN_ALERTS channel is configured. Paid digests are sent only by the
// durable outbox worker, to the private C.A.S.E. status channel. Free operational alerts are
// unaffected.
func TestAdminAlertsNeverPublishesPaidWatchDigest(t *testing.T) {
	p, sender, resolver, _ := newAlertFixture()
	resolver.set(7, 30, routeKeyAdminAlerts, "admin-alerts-30")
	p.Publish(AdminAlert{GuildRowID: 7, ServerID: 30, Kind: AlertKindCaseWatchDigest, Headline: "digest",
		Fields: [][2]string{{"Persisted source lines", "5"}}})
	drain(p)
	if sender.total() != 0 {
		t.Fatalf("ADMIN_ALERTS published a paid Watch digest: %d", sender.total())
	}
	p.Publish(AdminAlert{GuildRowID: 7, ServerID: 30, Kind: AlertKindADMStale, Headline: "ADM STALE"})
	drain(p)
	if sender.total() != 1 || len(sender.messages("admin-alerts-30")) != 1 {
		t.Fatalf("free ADM alerts must remain unaffected: %d", sender.total())
	}
}

func TestCaseWatchDigestEmbedIsObservationalOnly(t *testing.T) {
	embed := BuildCaseWatchDigestEmbed(AdminAlert{ServerID: 30, Kind: AlertKindCaseWatchDigest, At: time.Now(),
		Fields: [][2]string{{"Persisted source lines", "5"}, {"Coverage", "Source observations only"}, {"", "skipped"}}}, "Chernarus #1")
	if !strings.Contains(embed.Title, "OBSERVATION DIGEST") || !strings.Contains(embed.Description, "not cheat alerts") ||
		strings.Contains(embed.Description, "confirmed cheat") {
		t.Fatalf("digest mislabeled: %+v", embed)
	}
	if len(embed.Fields) != 3 || embed.Fields[0].Name != "Server" {
		t.Fatalf("server and source-count fields: %+v", embed.Fields)
	}
	if embed.Footer == nil || !strings.Contains(embed.Footer.Text, "NO ENFORCEMENT") {
		t.Fatalf("footer: %+v", embed.Footer)
	}
}
