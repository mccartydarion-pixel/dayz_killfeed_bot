package killfeed

import (
	"context"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

const (
	hitLineTorso = `16:30:00 | Player "Victim" (id=v001 pos=<7504.7, 1334.4, 0.9>) [HP: 42.5] hit by Player "Shooter" (id=s002 pos=<7490.2, 1528.2, 43.0>) into Torso(3) for 28.8605 damage (Bullet_762x39Tracer) with testing AKM from 115.058 meters`
	// Same second, same weapon and distance, different zone and damage.
	hitLineHead = `16:30:00 | Player "Victim" (id=v001 pos=<7504.7, 1334.4, 0.9>) [HP: 12.0] hit by Player "Shooter" (id=s002 pos=<7490.2, 1528.2, 43.0>) into Head(1) for 30.5000 damage (Bullet_762x39Tracer) with testing AKM from 115.058 meters`
	// The killing blow: victim already (DEAD), followed by the explicit kill line.
	hitLineLethal = `16:30:02 | Player "Victim" (DEAD) (id=v001 pos=<7504.7, 1334.4, 0.9>) [HP: 0] hit by Player "Shooter" (id=s002 pos=<7490.2, 1528.2, 43.0>) into Head(1) for 50.0000 damage (Bullet_762x39Tracer) with testing AKM from 115.058 meters`
	killLineAfter = `16:30:02 | Player "Victim" (DEAD) (id=v001 pos=<7504.7, 1334.4, 0.9>) killed by Player "Shooter" (id=s002 pos=<7490.2, 1528.2, 43.0>) with testing AKM from 115.058 meters`
)

type recordingHitPublisher struct{ hits []*Event }

func (r *recordingHitPublisher) PublishHit(ev *Event) { r.hits = append(r.hits, ev) }

type panickingHitPublisher struct{}

func (panickingHitPublisher) PublishHit(*Event) { panic("hit consumer bug") }

func hitEngine(t *testing.T) *Engine {
	t.Helper()
	fake := &fakeLogSource{
		logs:    []nitrado.LogFile{{Name: "DayZServer_PS4_x64_test.ADM", Path: "/logs/test.ADM", Size: 1, Modified: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC), Type: "ADM"}},
		content: []byte("x\n"),
	}
	e := NewEngine(fake, "svc-1", NewADMParser())
	_ = e.PollOnce(context.Background()) // discovery -> select
	return e
}

// Every non-duplicate hit line reaches the HitPublisher with its parsed fields.
func TestEngineDeliversParsedHitToHitPublisher(t *testing.T) {
	e := hitEngine(t)
	pub := &recordingHitPublisher{}
	e.SetHitPublisher(pub)

	e.processLines([]string{hitLineTorso})

	if len(pub.hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(pub.hits))
	}
	h := pub.hits[0]
	if h.Type != EventPlayerHit || h.Attacker.Name != "Shooter" || h.Victim.Name != "Victim" || h.HitZone != "Torso" || h.Weapon != "testing AKM" || h.Distance == nil || h.Damage == nil {
		t.Fatalf("unexpected hit: %+v", h)
	}
	if e.Metrics().HitsParsed != 1 {
		t.Fatalf("HitsParsed must still count the hit, got %d", e.Metrics().HitsParsed)
	}
}

// A hit followed by the kill: the hit goes to the HitPublisher and the kill
// ONLY to the KillPublisher - the hit feed never receives (or renders) a kill.
func TestEngineHitThenKillKeepsFeedRolesSeparate(t *testing.T) {
	e := hitEngine(t)
	hits := &recordingHitPublisher{}
	kills := &recordingPublisher{}
	e.SetHitPublisher(hits)
	e.SetKillPublisher(kills)

	e.processLines([]string{hitLineLethal, killLineAfter})

	if len(hits.hits) != 1 || hits.hits[0].Type != EventPlayerHit {
		t.Fatalf("expected exactly the one PLAYER_HIT on the hit feed, got %d", len(hits.hits))
	}
	if len(kills.kills) != 1 || kills.kills[0].Type != EventPlayerKill {
		t.Fatalf("expected exactly one kill on the kill feed, got %d", len(kills.kills))
	}
}

// A replayed line (retry after a later persistence failure, rotation overlap)
// is dropped by the ADM dedupe before it can reach the hit feed twice.
func TestEngineReplayedHitIsNotPublishedTwice(t *testing.T) {
	e := hitEngine(t)
	pub := &recordingHitPublisher{}
	e.SetHitPublisher(pub)

	e.processLines([]string{hitLineTorso})
	e.processLines([]string{hitLineTorso})

	if len(pub.hits) != 1 {
		t.Fatalf("a replayed hit must be published once, got %d", len(pub.hits))
	}
	if e.Metrics().DuplicateEventsDropped != 1 {
		t.Fatalf("expected the replay counted as a duplicate, got %d", e.Metrics().DuplicateEventsDropped)
	}
}

// Two genuinely different hits in the same second with the same weapon and
// distance (auto-fire at a stationary target) must both get through: the hit
// fingerprint includes zone and damage.
func TestEngineDistinctRapidHitsAreNotCollapsedByDedupe(t *testing.T) {
	e := hitEngine(t)
	pub := &recordingHitPublisher{}
	e.SetHitPublisher(pub)

	e.processLines([]string{hitLineTorso, hitLineHead})

	if len(pub.hits) != 2 {
		t.Fatalf("expected both distinct hits, got %d", len(pub.hits))
	}
}

// A broken hit consumer can neither stop the parse loop nor affect kills.
func TestEngineSurvivesPanickingHitPublisher(t *testing.T) {
	e := hitEngine(t)
	kills := &recordingPublisher{}
	e.SetHitPublisher(panickingHitPublisher{})
	e.SetKillPublisher(kills)

	parsed := e.processLines([]string{hitLineTorso, hitLineHead, killLineAfter})

	if parsed != 3 {
		t.Fatalf("expected all 3 lines processed despite the panic, got %d", parsed)
	}
	if len(kills.kills) != 1 {
		t.Fatalf("kill processing must be unaffected, got %d kills", len(kills.kills))
	}
}

// With no hit publisher attached behaviour is exactly as before: hits counted only.
func TestEngineWithoutHitPublisherOnlyCountsHits(t *testing.T) {
	e := hitEngine(t)
	e.processLines([]string{hitLineTorso})
	if e.Metrics().HitsParsed != 1 {
		t.Fatalf("expected the hit counted, got %d", e.Metrics().HitsParsed)
	}
}

func TestHitFingerprintStillIgnoresKillsFields(t *testing.T) {
	// Kill fingerprints are untouched by the hit-specific additions.
	a := killEvent("V", "K", "M4-A1", 62.1978, "16:40:12")
	b := killEvent("V", "K", "M4-A1", 62.1978, "16:40:12")
	if fingerprint(a) != fingerprint(b) {
		t.Fatal("identical kills must keep an identical fingerprint")
	}
}
