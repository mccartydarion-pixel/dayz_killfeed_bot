package killfeed

import (
	"context"
	"errors"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

type caseEvidenceRecorder struct {
	items []repository.CaseEvidenceInput
	err error
}
func (s *caseEvidenceRecorder) RecordCaseEvidence(_ context.Context, in repository.CaseEvidenceInput) error {
	if s.err!=nil {return s.err}
	s.items=append(s.items,in)
	return nil
}

const caseTestHit = `16:30:00 | Player "Victim" (id=v001 pos=<7504.7, 1334.4, 0.9>) [HP: 42.5] hit by Player "Shooter" (id=s002 pos=<7490.2, 1528.2, 43.0>) into Torso(3) for 28.8605 damage (Bullet_762x39Tracer) with AKM from 115.058 meters`

func TestCaseEvidenceIdenticalHitLinesRetainSeparateSourceOffsets(t *testing.T) {
	e:=NewEngine(nil,"svc",NewADMParser())
	e.SetDurableCheckpoint(nil,11,22)
	store:=&caseEvidenceRecorder{}
	e.SetEvidenceStore(store)
	p1:="/games/123/noftp/dayzps/config/DayZServer_PS4_x64_2026-09-23.ADM"
	p2:="/games/123/ftproot/dayzps/config/DayZServer_PS4_x64_2026-09-23.ADM"
	if _,err:=e.processLineAt(caseTestHit,p1,100);err!=nil {t.Fatal(err)}
	if _,err:=e.processLineAt(caseTestHit,p2,300);err!=nil {t.Fatal(err)}
	if len(store.items)!=2 {t.Fatalf("both source lines must be observed despite legacy fingerprint dedupe, got %d",len(store.items))}
	a,b:=store.items[0],store.items[1]
	if a.SourceID!=b.SourceID || a.SourceEndOffset!=100 || b.SourceEndOffset!=300 || a.LineSHA256!=b.LineSHA256 {
		t.Fatalf("source provenance invalid: a=%+v b=%+v",a,b)
	}
	if a.Actor.DayZID!="s002" || a.Target.DayZID!="v001" || a.Actor.X==nil || *a.Actor.X!=7490.2 || a.Actor.Z==nil || *a.Actor.Z!=1528.2 || a.Actor.Altitude==nil || *a.Actor.Altitude!=43.0 {
		t.Fatalf("hit identity/ADM coordinate order wrong: %+v",a)
	}
	if a.ADMClock!="16:30:00" || a.BoundaryKind!="" || a.Damage==nil || *a.Damage!=28.8605 {
		t.Fatalf("invented timing or missing hit metadata: %+v",a)
	}
}

func TestCaseEvidenceFailureStopsBeforeLegacyDedupe(t *testing.T) {
	e:=NewEngine(nil,"svc",NewADMParser())
	e.SetDurableCheckpoint(nil,11,22)
	store:=&caseEvidenceRecorder{err:errors.New("temporary storage failure")}
	e.SetEvidenceStore(store)
	if _,err:=e.processLineAt(caseTestHit,"/noftp/dayzps/config/test.ADM",400);err==nil {t.Fatal("checkpoint must not advance past missing evidence")}
	store.err=nil
	if _,err:=e.processLineAt(caseTestHit,"/ftproot/dayzps/config/test.ADM",400);err!=nil {t.Fatal(err)}
	if len(store.items)!=1 {t.Fatalf("retry must reach evidence sink, got %d",len(store.items))}
}

func TestCaseBoundaryOnlyForProvenLifecycle(t *testing.T) {
	for _,tt:=range []struct{kind EventType; expected string}{
		{EventPlayerConnect,"CONNECT"},{EventPlayerDisconnect,"DISCONNECT"},
		{EventPlayerRespawn,"RESPAWN"},{EventPlayerKill,"DEATH"},
		{EventPlayerDeath,"DEATH"},{EventSuicideAction,"SUICIDE"},
		{EventPlayerHit,""},{EventPlayerUnconscious,""},{EventPlayerConscious,""},
	} {
		got:=caseBoundary(&Event{Type:tt.kind})
		if got!=tt.expected {t.Fatalf("%s -> %q, expected %q",tt.kind,got,tt.expected)}
	}
}
