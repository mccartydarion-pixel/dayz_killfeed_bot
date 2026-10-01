package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/caseintel"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type fakeCaseAlertStore struct {
	enabled  bool
	servers  []repository.CaseAlertServer
	players  []int64
	queued   []caseintel.Finding
	due      []repository.CaseAlertDelivery
	sent     map[int64]string
	retried  map[int64]string
	dead     map[int64]string
	claimErr error
}

func (f *fakeCaseAlertStore) GetSettings(context.Context, int64, int64, int64) (repository.CaseAlertSettings, error) {
	return repository.CaseAlertSettings{Enabled: f.enabled}, nil
}
func (f *fakeCaseAlertStore) EnabledServers(context.Context) ([]repository.CaseAlertServer, error) {
	return f.servers, nil
}
func (f *fakeCaseAlertStore) RecentSessionPlayers(context.Context, int64, int64, time.Time, int) ([]int64, error) {
	return f.players, nil
}
func (f *fakeCaseAlertStore) Enqueue(_ context.Context, _ repository.CaseAlertServer, fd caseintel.Finding) (bool, error) {
	f.queued = append(f.queued, fd)
	return true, nil
}
func (f *fakeCaseAlertStore) ClaimDue(context.Context, int, time.Duration) ([]repository.CaseAlertDelivery, error) {
	due := f.due
	f.due = nil
	return due, f.claimErr
}
func (f *fakeCaseAlertStore) MarkSent(_ context.Context, id int64, channel, _ string) error {
	f.init()
	f.sent[id] = channel
	return nil
}
func (f *fakeCaseAlertStore) MarkRetry(_ context.Context, id int64, code string, _ time.Duration) error {
	f.init()
	f.retried[id] = code
	return nil
}
func (f *fakeCaseAlertStore) MarkDead(_ context.Context, id int64, code string) error {
	f.init()
	f.dead[id] = code
	return nil
}
func (f *fakeCaseAlertStore) init() {
	if f.sent == nil {
		f.sent, f.retried, f.dead = map[int64]string{}, map[int64]string{}, map[int64]string{}
	}
}

var caseAlertTestScope = caseintel.Core8Scope{GuildID: 3, InstallationID: 2, ServerID: 4}

func caseAlertFinding(tier caseintel.EvidenceTier) caseintel.Finding {
	return caseintel.Finding{DetectorID: caseAlertModuleID, Scope: caseAlertTestScope, PlayerID: 42, PlayerName: "@everyone Bob",
		EvidenceIDs: []int64{7, 8}, EventAt: time.Unix(1_800_000_000, 0), ObservedAt: time.Unix(1_800_000_030, 0),
		Behavior: "Reconnected again and again", Explanation: "Reconnected 5 times in 8 minutes.", Tier: tier,
		IncidentKey: strings.Repeat("ab", 32)}
}

func newCaseAlertWorker(store *fakeCaseAlertStore, d *designerDiscordFake, released bool, res caseintel.Core8Result) (*caseAlertWorker, *int) {
	evaluated := 0
	return &caseAlertWorker{
		store: store, routes: &designerRoutesFake{routes: map[string]string{caseAlertsRoute: "alerts"}}, discord: d,
		released: func(string) bool { return released },
		evaluate: func(context.Context, repository.CaseAlertServer, int64) (caseintel.Core8Result, error) {
			evaluated++
			return res, nil
		},
		serverName: func(int64) string { return "Champions #1" }, siteURL: "https://champions.example", now: time.Now,
	}, &evaluated
}

func TestCaseAlertWorkerScansOnlyReleasedDetectors(t *testing.T) {
	server := repository.CaseAlertServer{InstallationID: 2, GuildID: 3, ServerID: 4}
	notify := caseintel.Core8Result{CanNotify: true, Findings: []caseintel.Finding{caseAlertFinding(caseintel.TierSuspicious), caseAlertFinding(caseintel.TierObserved)}}

	store := &fakeCaseAlertStore{servers: []repository.CaseAlertServer{server}, players: []int64{42}}
	w, evaluated := newCaseAlertWorker(store, caseAlertGuild(true, false, false), false, notify)
	w.tick(context.Background())
	if *evaluated != 0 || len(store.queued) != 0 {
		t.Fatalf("unreleased detector was evaluated or queued: %d %d", *evaluated, len(store.queued))
	}

	w, evaluated = newCaseAlertWorker(store, caseAlertGuild(true, false, false), true, notify)
	w.tick(context.Background())
	if *evaluated != 1 || len(store.queued) != 1 || store.queued[0].Tier != caseintel.TierSuspicious {
		t.Fatalf("released detector should queue only the suspicious finding: %d %+v", *evaluated, store.queued)
	}

	store.queued = nil
	quiet := notify
	quiet.CanNotify = false
	w, _ = newCaseAlertWorker(store, caseAlertGuild(true, false, false), true, quiet)
	w.tick(context.Background())
	if len(store.queued) != 0 {
		t.Fatal("a result that may not notify was queued")
	}
}

func TestCaseAlertWorkerDelivery(t *testing.T) {
	delivery := func(id int64) repository.CaseAlertDelivery {
		return repository.CaseAlertDelivery{ID: id, InstallationID: 2, GuildID: 3, ServerID: 4, OrganizationID: 1,
			DiscordGuildID: "guild", DetectorID: caseAlertModuleID, Attempts: 1, Finding: caseAlertFinding(caseintel.TierSuspicious)}
	}

	store := &fakeCaseAlertStore{enabled: true, due: []repository.CaseAlertDelivery{delivery(1)}}
	d := caseAlertGuild(true, false, false)
	w, _ := newCaseAlertWorker(store, d, true, caseintel.Core8Result{})
	w.tick(context.Background())
	if store.sent[1] != "alerts" || len(d.sent) != 1 {
		t.Fatalf("alert not sent to the private channel: %+v", store)
	}
	msg := d.sent[0].msg
	e := msg.Embeds[0]
	if msg.AllowedMentions == nil || len(msg.AllowedMentions.Parse) != 0 {
		t.Fatal("mentions must be disabled")
	}
	if e.URL != "https://champions.example/dashboard/anti-cheat?tab=players&playerId=42" || !strings.Contains(e.Title, "needs a staff look") {
		t.Fatalf("card: %q %q", e.URL, e.Title)
	}
	for _, f := range e.Fields {
		if strings.Contains(f.Value, "@everyone") {
			t.Fatalf("player text not neutralised: %q", f.Value)
		}
	}

	cases := map[string]struct {
		enabled  bool
		released bool
		guild    *designerDiscordFake
		mutate   func(*repository.CaseAlertDelivery)
		wantDead string
		wantRetr string
	}{
		"owner turned alerts off": {false, true, caseAlertGuild(true, false, false), nil, caseAlertOwnerOff, ""},
		"detector un-released":    {true, false, caseAlertGuild(true, false, false), nil, caseAlertNotReleased, ""},
		"finding for another tenant": {true, true, caseAlertGuild(true, false, false), func(d *repository.CaseAlertDelivery) {
			d.Finding.Scope.InstallationID = 99
		}, caseAlertBadFinding, ""},
		"public channel": {true, true, caseAlertGuild(false, false, false), nil, "", codeEmbedSendForbidden},
		"discord refuses": {true, true, func() *designerDiscordFake {
			g := caseAlertGuild(true, false, false)
			g.sendErr = errors.New("boom")
			return g
		}(), nil, "", caseAlertSendRejected},
	}
	for name, tc := range cases {
		item := delivery(5)
		if tc.mutate != nil {
			tc.mutate(&item)
		}
		store := &fakeCaseAlertStore{enabled: tc.enabled, due: []repository.CaseAlertDelivery{item}}
		w, _ := newCaseAlertWorker(store, tc.guild, tc.released, caseintel.Core8Result{})
		w.tick(context.Background())
		if len(tc.guild.sent) != 0 || store.sent[5] != "" {
			t.Fatalf("%s: alert was sent", name)
		}
		if store.dead[5] != tc.wantDead || store.retried[5] != tc.wantRetr {
			t.Fatalf("%s: dead=%q retry=%q", name, store.dead[5], store.retried[5])
		}
	}
}

func TestCaseAlertBackoffAndReviewURL(t *testing.T) {
	if caseAlertBackoff(1) != time.Minute || caseAlertBackoff(3) != 9*time.Minute || caseAlertBackoff(0) != time.Minute {
		t.Fatal("backoff")
	}
	w := &caseAlertWorker{siteURL: "http://insecure.example"}
	if w.reviewURL(1) != "" {
		t.Fatal("non-https review link accepted")
	}
}

func TestNoDetectorIsReleasedYet(t *testing.T) {
	if got := releasedCaseModules(); len(got) != 0 {
		t.Fatalf("a detector was released without review: %v", got)
	}
}
