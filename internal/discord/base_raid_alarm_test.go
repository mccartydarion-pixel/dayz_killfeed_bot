package discord

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type fakeRaidStore struct {
	matches    []repository.BaseRaidMatch
	nextID     int64
	matchCalls int
	gotRaider  string
	gotX, gotZ float64
	recorded   []repository.BaseRaidEvent
	delivery   map[int64]string
}

func (f *fakeRaidStore) MatchRaid(_ context.Context, _, _ int64, raider string, x, z float64) ([]repository.BaseRaidMatch, error) {
	f.matchCalls++
	f.gotRaider, f.gotX, f.gotZ = raider, x, z
	return f.matches, nil
}

func (f *fakeRaidStore) RecordAlert(_ context.Context, _ repository.BaseRaidMatch, ev repository.BaseRaidEvent) (int64, error) {
	f.recorded = append(f.recorded, ev)
	return f.nextID, nil
}

func (f *fakeRaidStore) MarkDelivery(_ context.Context, id int64, d string) error {
	if f.delivery == nil {
		f.delivery = map[int64]string{}
	}
	f.delivery[id] = d
	return nil
}

type fakeDM struct {
	err     error
	to      []string
	message *discordgo.MessageSend
}

func (f *fakeDM) UserChannelCreate(id string, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.to = append(f.to, id)
	return &discordgo.Channel{ID: "dm-" + id}, nil
}

func (f *fakeDM) ChannelMessageSendComplex(_ string, m *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.message = m
	if f.err != nil {
		return nil, f.err
	}
	return &discordgo.Message{ID: "m1"}, nil
}

func dismantle(action string, pos *killfeed.Position) *killfeed.Event {
	return &killfeed.Event{Type: killfeed.EventBuildAction, TimeOfDay: "14:32:10",
		Player: &killfeed.PlayerRef{Name: "Raider", ID: "adm-raider", Position: pos},
		Build:  &killfeed.BuildAction{Action: action, Object: "Wall", Target: "Fence", Tool: "Hatchet"}}
}

func drainRaid(p *BaseRaidAlarmPublisher) {
	for {
		select {
		case job := <-p.queue:
			p.handle(context.Background(), job)
		default:
			return
		}
	}
}

func TestBaseRaidAlarmOnlyQueuesDismantlesWithPlayerAndPosition(t *testing.T) {
	store := &fakeRaidStore{}
	p := NewBaseRaidAlarmPublisher(store, &fakeDM{}, 1, 2)
	pos := &killfeed.Position{X: 1000, Y: 2000, Z: 150}
	p.PublishBuild(dismantle("Built", pos))
	p.PublishBuild(dismantle("Placed", pos))
	p.PublishBuild(dismantle("Dismantled", nil))
	noID := dismantle("Dismantled", pos)
	noID.Player.ID = ""
	p.PublishBuild(noID)
	p.PublishBuild(nil)
	drainRaid(p)
	if store.matchCalls != 0 {
		t.Fatalf("non-dismantle or incomplete lines reached the store: %d", store.matchCalls)
	}
	p.PublishBuild(dismantle("Dismantled", pos))
	drainRaid(p)
	// ADM pos=<x, z, altitude>: map X is the first value, map Z the second.
	if store.matchCalls != 1 || store.gotRaider != "adm-raider" || store.gotX != 1000 || store.gotZ != 2000 {
		t.Fatalf("match call: %+v", store)
	}
}

func TestBaseRaidAlarmDeliveryOutcomes(t *testing.T) {
	pos := &killfeed.Position{X: 1000, Y: 2000}
	linked := repository.BaseRaidMatch{BaseID: 7, BaseName: "North Base", OwnerDiscordUserID: "owner-1"}

	store, dm := &fakeRaidStore{matches: []repository.BaseRaidMatch{linked}, nextID: 11}, &fakeDM{}
	p := NewBaseRaidAlarmPublisher(store, dm, 1, 2)
	p.SetServerName(func(int64) string { return "Champions #1" })
	p.PublishBuild(dismantle("Dismantled", pos))
	drainRaid(p)
	if store.delivery[11] != repository.BaseRaidDeliverySent || len(dm.to) != 1 || dm.to[0] != "owner-1" {
		t.Fatalf("linked owner: %+v %+v", store.delivery, dm.to)
	}
	if ev := store.recorded[0]; ev.RaiderName != "Raider" || ev.Part != "Wall" || ev.TimeOfDay != "14:32:10" {
		t.Fatalf("recorded event: %+v", ev)
	}

	store, dm = &fakeRaidStore{matches: []repository.BaseRaidMatch{linked}, nextID: 0}, &fakeDM{}
	p = NewBaseRaidAlarmPublisher(store, dm, 1, 2)
	p.PublishBuild(dismantle("Dismantled", pos))
	drainRaid(p)
	if len(dm.to) != 0 || len(store.delivery) != 0 {
		t.Fatal("an alarm inside the cooldown was delivered")
	}

	unlinked := linked
	unlinked.OwnerDiscordUserID = ""
	store, dm = &fakeRaidStore{matches: []repository.BaseRaidMatch{unlinked}, nextID: 12}, &fakeDM{}
	p = NewBaseRaidAlarmPublisher(store, dm, 1, 2)
	p.PublishBuild(dismantle("Dismantled", pos))
	drainRaid(p)
	if store.delivery[12] != repository.BaseRaidDeliveryOwnerNotLinked || len(dm.to) != 0 {
		t.Fatalf("unlinked owner: %+v", store.delivery)
	}

	store, dm = &fakeRaidStore{matches: []repository.BaseRaidMatch{linked}, nextID: 13}, &fakeDM{err: &discordgo.RESTError{Response: &http.Response{StatusCode: 403}, Message: &discordgo.APIErrorMessage{Code: 50007}}}
	p = NewBaseRaidAlarmPublisher(store, dm, 1, 2)
	p.PublishBuild(dismantle("Dismantled", pos))
	drainRaid(p)
	if store.delivery[13] != repository.BaseRaidDeliveryFailed {
		t.Fatalf("closed DMs: %+v", store.delivery)
	}
}

func TestBaseRaidAlarmQueueIsBounded(t *testing.T) {
	p := NewBaseRaidAlarmPublisher(&fakeRaidStore{}, &fakeDM{}, 1, 2)
	pos := &killfeed.Position{X: 1, Y: 2}
	for i := 0; i < baseRaidQueueCap+5; i++ {
		p.PublishBuild(dismantle("Dismantled", pos))
	}
	if p.Dropped() != 5 {
		t.Fatalf("dropped = %d", p.Dropped())
	}
}

func TestBaseRaidAlarmMessageIsPlainAndSafe(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	msg := BaseRaidAlarmMessage("My @everyone Base", "Champions #1",
		repository.BaseRaidEvent{RaiderName: "@here `Bob`", Part: "Wall", Target: "Fence", Tool: "Hatchet", TimeOfDay: "14:32:10"}, at)
	if msg.AllowedMentions == nil || len(msg.AllowedMentions.Parse) != 0 {
		t.Fatal("mentions must be disabled")
	}
	e := msg.Embeds[0]
	all := e.Title + e.Description
	for _, f := range e.Fields {
		all += f.Name + f.Value
	}
	if strings.Contains(all, "@everyone") || strings.Contains(all, "@here") || strings.Contains(all, "`") {
		t.Fatalf("unsafe text in DM: %q", all)
	}
	for _, want := range []string{"Someone is breaking into My", "Took apart **Wall** from Fence with a Hatchet", "14:32:10 server time", "Champions #1"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in %q", want, all)
		}
	}
}
