package discord

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

func buildEvent(player, line string) *killfeed.Event {
	ev, _ := (&killfeed.ADMParser{}).ParseLine(line)
	return ev
}

func fieldMap(e *discordgo.MessageEmbed) map[string]string {
	out := map[string]string{}
	for _, f := range e.Fields {
		out[f.Name] = f.Value
	}
	return out
}

func TestBuildFeedCardsCarryOnlyRealFields(t *testing.T) {
	sender := &fakeHitSender{}
	resolver := newKeyedResolver()
	resolver.set(7, 1, routeKeyBuildFeed, "chan-admin")
	p := NewBuildFeedPublisher(sender, resolver, 7, 1)
	p.SetServerName(func(int64) string { return "Champions" })
	seen := 0
	p.OnSeen(func() { seen++ })

	p.PublishBuild(buildEvent("", `18:05:12 | Player "Survivor" (id=a pos=<5420.3, 8931.2, 312.1>) placed Sea Chest<SeaChest>`))
	p.PublishBuild(buildEvent("", `18:07:00 | Player "Raider" (id=r) Dismantled Wall Upper from Fence with Hatchet`))
	p.Flush(context.Background())

	msgs := sender.messages("chan-admin")
	if len(msgs) != 1 || len(msgs[0].embeds) != 2 || seen != 2 {
		t.Fatalf("want one message with two cards, got %d messages (seen=%d)", len(msgs), seen)
	}
	placed := fieldMap(msgs[0].embeds[0])
	if msgs[0].embeds[0].Title != "🏗️ BUILD ACTIVITY" || placed["Player"] != "Survivor" || placed["Action"] != "Placed" || placed["Object"] != "Sea Chest" ||
		placed["Location"] != "X: 5,420 • Z: 8,931" || placed["Server"] != "Champions" {
		t.Fatalf("placed card: %+v", placed)
	}
	if _, ok := placed["Tool"]; ok {
		t.Fatal("a field the ADM line did not carry must be omitted")
	}
	dismantled := fieldMap(msgs[0].embeds[1])
	if dismantled["From"] != "Fence" || dismantled["Tool"] != "Hatchet" {
		t.Fatalf("dismantled card: %+v", dismantled)
	}
	if _, ok := dismantled["Location"]; ok {
		t.Fatal("no pos in the line: no location")
	}
}

func TestBuildFeedBurstBecomesOneSummary(t *testing.T) {
	sender := &fakeHitSender{}
	resolver := newKeyedResolver()
	resolver.set(7, 1, routeKeyBuildFeed, "chan-admin")
	p := NewBuildFeedPublisher(sender, resolver, 7, 1)
	for i := 0; i < 14; i++ {
		p.PublishBuild(buildEvent("", fmt.Sprintf(`18:06:%02d | Player "Builder" (id=b) Built Wall %d on Fence with Hammer`, i, i)))
	}
	p.Flush(context.Background())
	msgs := sender.messages("chan-admin")
	if len(msgs) != 1 || len(msgs[0].embeds) != 1 {
		t.Fatalf("a burst must be one summary card, got %d messages", len(msgs))
	}
	d := msgs[0].embeds[0].Description
	if strings.Count(d, "**Builder** built") != buildFeedSummaryLines || !strings.Contains(d, "+4 more actions not shown") {
		t.Fatalf("summary: %s", d)
	}
}

func TestBuildFeedNoRouteSendsNothing(t *testing.T) {
	sender := &fakeHitSender{}
	p := NewBuildFeedPublisher(sender, newKeyedResolver(), 7, 1)
	p.PublishBuild(buildEvent("", `18:05:12 | Player "Survivor" (id=a) placed Sea Chest`))
	p.Flush(context.Background())
	if sender.total() != 0 {
		t.Fatal("no BUILD_FEED route: nothing is sent")
	}
	var nilP *BuildFeedPublisher
	nilP.PublishBuild(nil)
	nilP.Flush(context.Background())
}
