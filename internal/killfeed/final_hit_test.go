package killfeed

import (
	"testing"
)

// Final-hit correlation, on the exact line shapes Champions' ADM wrote on 2026-09-24 (names and
// ids replaced): the lethal hit, marked (DEAD) with HP 0, immediately before the kill line.

const (
	fhPath       = "/games/svc_2/noftp/dayzps/config/DayZServer_PS4_x64_2026-09-24_09-17-09.ADM"
	fhHitHealthy = `09:33:17 | Player "Victim" (id=v001 pos=<4621.1, 8397.2, 319.6>)[HP: 18.5404] hit by Player "Killer" (id=k002 pos=<4600.0, 8390.0, 320.0>) into Torso(1) for 26.4821 damage (Bullet_556x45) with M4-A1 from 74.0376 meters`
	fhHitLethal  = `09:33:17 | Player "Victim" (DEAD) (id=v001 pos=<4621.1, 8397.2, 319.6>)[HP: 0] hit by Player "Killer" (id=k002 pos=<4600.0, 8390.0, 320.0>) into Head(0) for 22.0527 damage (Bullet_556x45) with M4-A1 from 74.9605 meters`
	fhKill       = `09:33:17 | Player "Victim" (DEAD) (id=v001 pos=<4621.1, 8397.2, 319.6>) killed by Player "Killer" (id=k002 pos=<4600.0, 8390.0, 320.0>) with M4-A1 from 74.9605 meters`
)

type recordingKills struct{ events []*Event }

func (r *recordingKills) PublishKill(ev *Event) error { r.events = append(r.events, ev); return nil }

// feedLines processes lines as one contiguous file, with real end offsets.
func feedLines(t *testing.T, e *Engine, path string, start int64, lines ...string) int64 {
	t.Helper()
	off := start
	for _, l := range lines {
		off += int64(len(l)) + 1
		if _, err := e.processLineAt(l, path, off); err != nil {
			t.Fatal(err)
		}
	}
	return off
}

func newFinalHitEngine() (*Engine, *recordingKills) {
	e := NewEngine(nil, "svc", NewADMParser())
	pub := &recordingKills{}
	e.SetKillPublisher(pub)
	return e, pub
}

func TestFinalHitCorrelatedFromTheImmediatelyPrecedingLethalHit(t *testing.T) {
	e, pub := newFinalHitEngine()
	feedLines(t, e, fhPath, 0, fhHitHealthy, fhHitLethal, fhKill)
	if len(pub.events) != 1 {
		t.Fatalf("one kill: %d", len(pub.events))
	}
	k := pub.events[0]
	if k.FinalHit == nil || k.FinalHit.Zone != "Head" || k.FinalHit.Damage == nil || *k.FinalHit.Damage != 22.0527 {
		t.Fatalf("the lethal hit (not the earlier torso hit) is attached: %+v", k.FinalHit)
	}
	// The kill's own hit fields - headshot statistics, the default card, the durable fingerprint -
	// are untouched.
	if k.HitZone != "" || k.Damage != nil || isHeadshotEvent(k) {
		t.Fatalf("kill hit fields must stay empty: zone=%q damage=%v", k.HitZone, k.Damage)
	}
}

func TestFinalHitRejectedWhenEvidenceDisagrees(t *testing.T) {
	other := `09:33:17 | Player "Someone" (id=x003 pos=<1.0, 2.0, 3.0>) is connected`
	cases := []struct {
		name  string
		lines []string
	}{
		{"not lethal (no DEAD marker)", []string{fhHitHealthy, fhKill}},
		{"another line in between", []string{fhHitLethal, other, fhKill}},
		{"different killer", []string{
			`09:33:17 | Player "Victim" (DEAD) (id=v001 pos=<1.0, 2.0, 3.0>)[HP: 0] hit by Player "Third" (id=t009 pos=<1.0, 2.0, 3.0>) into Head(0) for 22.0527 damage (Bullet_556x45) with M4-A1 from 74.9605 meters`, fhKill}},
		{"different victim", []string{
			`09:33:17 | Player "Other" (DEAD) (id=o007 pos=<1.0, 2.0, 3.0>)[HP: 0] hit by Player "Killer" (id=k002 pos=<1.0, 2.0, 3.0>) into Head(0) for 22.0527 damage (Bullet_556x45) with M4-A1 from 74.9605 meters`, fhKill}},
		{"different second", []string{
			`09:33:15 | Player "Victim" (DEAD) (id=v001 pos=<1.0, 2.0, 3.0>)[HP: 0] hit by Player "Killer" (id=k002 pos=<1.0, 2.0, 3.0>) into Head(0) for 22.0527 damage (Bullet_556x45) with M4-A1 from 74.9605 meters`, fhKill}},
		{"different weapon", []string{
			`09:33:17 | Player "Victim" (DEAD) (id=v001 pos=<1.0, 2.0, 3.0>)[HP: 0] hit by Player "Killer" (id=k002 pos=<1.0, 2.0, 3.0>) into Head(0) for 22.0527 damage (Bullet_308Win) with SCR 17 from 74.9605 meters`, fhKill}},
		{"different distance", []string{
			`09:33:17 | Player "Victim" (DEAD) (id=v001 pos=<1.0, 2.0, 3.0>)[HP: 0] hit by Player "Killer" (id=k002 pos=<1.0, 2.0, 3.0>) into Head(0) for 22.0527 damage (Bullet_556x45) with M4-A1 from 12.5 meters`, fhKill}},
	}
	for _, c := range cases {
		e, pub := newFinalHitEngine()
		feedLines(t, e, fhPath, 0, c.lines...)
		if len(pub.events) != 1 || pub.events[0].FinalHit != nil {
			t.Errorf("%s: no final hit may be attached: %+v", c.name, pub.events)
		}
	}
}

func TestFinalHitNeverCrossesFilesOrIsReused(t *testing.T) {
	// A lethal hit at the end of the previous boot's file and a kill at the start of the next boot:
	// different physical sources, never correlated.
	e, pub := newFinalHitEngine()
	end := feedLines(t, e, fhPath, 0, fhHitLethal)
	feedLines(t, e, "/games/svc_2/noftp/dayzps/config/DayZServer_PS4_x64_2026-09-24_10-13-02.ADM", 0, fhKill)
	if len(pub.events) != 1 || pub.events[0].FinalHit != nil {
		t.Fatalf("a hit from another boot file is never attached: %+v", pub.events[0].FinalHit)
	}
	_ = end
	// Single use: a second kill line cannot reuse the hit consumed by the first.
	e, pub = newFinalHitEngine()
	feedLines(t, e, fhPath, 0, fhHitLethal, fhKill, fhKill)
	if pub.events[0].FinalHit == nil || (len(pub.events) > 1 && pub.events[1].FinalHit != nil) {
		t.Fatalf("a lethal hit correlates with at most one kill: %+v", pub.events)
	}
}

func TestFinalHitReplayIsDeterministic(t *testing.T) {
	// Replaying the same bytes (a restart re-reading from its checkpoint) gives the same answer;
	// a replay that starts at the kill line (the hit before the checkpoint) attaches nothing.
	e, pub := newFinalHitEngine()
	feedLines(t, e, fhPath, 0, fhHitLethal, fhKill)
	e2, pub2 := newFinalHitEngine()
	feedLines(t, e2, fhPath, 0, fhHitLethal, fhKill)
	if pub.events[0].FinalHit == nil || pub2.events[0].FinalHit == nil || pub.events[0].FinalHit.Zone != pub2.events[0].FinalHit.Zone {
		t.Fatal("replaying the same bytes yields the same correlation")
	}
	e3, pub3 := newFinalHitEngine()
	feedLines(t, e3, fhPath, int64(len(fhHitLethal))+1, fhKill)
	if pub3.events[0].FinalHit != nil {
		t.Fatal("a kill whose hit was before the resume point has no final hit")
	}
}
