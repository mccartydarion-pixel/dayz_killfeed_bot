package discord

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type fakeFactionStore struct {
	ids    []string
	owners []int64
	shares []string
	sent   int
	failed int
}

func (f *fakeFactionStore) Recipients(_ context.Context, _, _, _, ownerPlayerID int64) ([]string, error) {
	f.owners = append(f.owners, ownerPlayerID)
	return f.ids, nil
}

func (f *fakeFactionStore) LogShare(_ context.Context, _, _, _, _ int64, source string, sent, failed int) error {
	f.shares = append(f.shares, source)
	f.sent, f.failed = sent, failed
	return nil
}

func TestPerimeterWatchSharesWithFaction(t *testing.T) {
	match := repository.PerimeterMatch{InstallationID: 1, GuildID: 1, ServerID: 2, BaseID: 7, BaseName: "North Base", OwnerPlayerID: 40, OwnerDiscordUserID: "owner", DistanceMeters: 120}
	store, dm := &fakePerimeterStore{matches: []repository.PerimeterMatch{match}, nextID: 3}, &fakeDM{}
	faction := &fakeFactionStore{ids: []string{"mate-1", "mate-2"}}
	p := NewPerimeterWatchPublisher(store, dm, 1, 2)
	p.SetFactionSecurity(faction)
	p.ObserveLocations([]killfeed.LocationSample{{ServerID: 2, PlayerID: 5, Gamertag: "Visitor", ObservedAt: time.Unix(1_800_000_000, 0)}})
	drainPerimeter(p)
	if len(dm.to) != 3 || dm.to[0] != "owner" || dm.to[1] != "mate-1" || dm.to[2] != "mate-2" {
		t.Fatalf("recipients: %v", dm.to)
	}
	if len(faction.owners) != 1 || faction.owners[0] != 40 || len(faction.shares) != 1 || faction.shares[0] != repository.FactionSharePerimeterWatch || faction.sent != 2 {
		t.Fatalf("share: %+v", faction)
	}
	e := dm.message.Embeds[0]
	if !strings.Contains(e.Title, "your faction's base North Base") || e.Footer.Text != "Shared with you by Faction Security" || len(dm.message.AllowedMentions.Parse) != 0 {
		t.Fatalf("faction message: %+v", e)
	}

	// A cooling-down alert is not shared either.
	store, dm, faction = &fakePerimeterStore{matches: []repository.PerimeterMatch{match}, nextID: 0}, &fakeDM{}, &fakeFactionStore{ids: []string{"mate-1"}}
	p = NewPerimeterWatchPublisher(store, dm, 1, 2)
	p.SetFactionSecurity(faction)
	p.ObserveLocations([]killfeed.LocationSample{{ServerID: 2, PlayerID: 5}})
	drainPerimeter(p)
	if len(dm.to) != 0 || len(faction.owners) != 0 {
		t.Fatalf("cooldown alert shared: %v %+v", dm.to, faction)
	}
}

func TestBaseRaidAlarmSharesWithFactionEvenWhenOwnerUnlinked(t *testing.T) {
	match := repository.BaseRaidMatch{InstallationID: 1, GuildID: 1, ServerID: 2, BaseID: 7, BaseName: "Den", OwnerPlayerID: 40}
	store := &fakeRaidStore{matches: []repository.BaseRaidMatch{match}, nextID: 5}
	dm, faction := &fakeDM{}, &fakeFactionStore{ids: []string{"mate-1"}}
	p := NewBaseRaidAlarmPublisher(store, dm, 1, 2)
	p.SetFactionSecurity(faction)
	p.PublishBuild(dismantle("Dismantled", &killfeed.Position{X: 1, Y: 2}))
	drainRaid(p)
	if len(dm.to) != 1 || dm.to[0] != "mate-1" || len(faction.shares) != 1 || faction.shares[0] != repository.FactionShareRaidAlarm {
		t.Fatalf("raid share: %v %+v", dm.to, faction)
	}
	if e := dm.message.Embeds[0]; !strings.Contains(e.Title, "your faction's base Den") || !strings.Contains(e.Description, "faction mate") {
		t.Fatalf("faction raid message: %+v", e)
	}
	// Without Faction Security nothing extra is sent.
	store, dm = &fakeRaidStore{matches: []repository.BaseRaidMatch{match}, nextID: 6}, &fakeDM{}
	p = NewBaseRaidAlarmPublisher(store, dm, 1, 2)
	p.SetFactionSecurity(nil)
	p.PublishBuild(dismantle("Dismantled", &killfeed.Position{X: 1, Y: 2}))
	drainRaid(p)
	if len(dm.to) != 0 {
		t.Fatalf("shared without Faction Security: %v", dm.to)
	}
}
