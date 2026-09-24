package caseintel

import (
	"reflect"
	"testing"
	"time"
)

func pid(n int64)*int64{return &n}
func ev(id, offset int64, file, kind string, subject,actor,target *int64)Event{
	return Event{ID:id,SourceID:file,SourceEndOffset:offset,Type:kind,
		ADMClock:"09:43:19",IngestedAt:time.Date(2026,9,24,13,51,25,0,time.UTC),
		SubjectID:subject,ActorID:actor,TargetID:target}
}
func has(values []string,want string)bool{for _,v:=range values{if v==want{return true}};return false}

func TestObservedKillHitOrderingDoesNotInventCheatOrTime(t *testing.T){
	// Real-world ADM shape: kill, unconscious, then separate hit source lines.
	// Ingestion order and clock are deliberately NOT used for chronology.
	file:="dayzps/config/boot.ADM"
	events:=[]Event{
		ev(4,43349,file,"PLAYER_HIT",nil,pid(2),pid(1)),
		ev(2,42737,file,"PLAYER_UNCONSCIOUS",pid(1),nil,nil),
		ev(3,43044,file,"PLAYER_HIT",nil,pid(2),pid(1)),
		ev(1,42610,file,"PLAYER_KILL",nil,pid(2),pid(1)),
	}
	r:=Reconstruct(1,events,200,false)
	if r.Mode!="RECONSTRUCTION_ONLY"||r.TimeBasis!=TimeBasis||r.DetectorsEnabled||r.Enforcement!="DISABLED"||!r.NoCrossSourceStitch||!r.NoDurationInference{t.Fatalf("unsafe contract: %+v",r)}
	if len(r.Sources)!=1||len(r.Sources[0].ConnectionWindows)!=1{t.Fatalf("unexpected groups: %+v",r)}
	w:=r.Sources[0].ConnectionWindows[0]
	got:=make([]int64,0,len(w.Observations))
	for _,o:=range w.Observations {got=append(got,o.SourceEndOffset)}
	if !reflect.DeepEqual(got,[]int64{42610,42737,43044,43349}){t.Fatalf("source offset order wrong: %v",got)}
	if w.Lives[0].EndReason!="RECORDED_DEATH"||w.Lives[0].StartReason!="FIRST_OBSERVED"{t.Fatalf("life inferred incorrectly: %+v",w.Lives)}
	if !has(w.Flags,"OBSERVATION_AFTER_RECORDED_DEATH_IN_SOURCE_ORDER") || !has(w.Flags,"MISSING_CONNECT_IN_WINDOW") {t.Fatalf("ambiguity not preserved: %+v",w)}
	if !has(w.Observations[2].Notes,"OBSERVATION_AFTER_RECORDED_DEATH_IN_SOURCE_ORDER"){t.Fatal("hit after source-ordered kill must be annotated as ordering ambiguity")}
	if len(w.Lives)!=1 {t.Fatal("late hit must not invent a new respawn/life")}
}

func TestActorKillNeverClosesActorsLife(t *testing.T) {
	file:="boot.ADM"
	r:=Reconstruct(2,[]Event{
		ev(1,100,file,"PLAYER_CONNECT",pid(2),nil,nil),
		ev(2,200,file,"PLAYER_KILL",nil,pid(2),pid(1)),
		ev(3,300,file,"PLAYER_HIT",nil,pid(2),pid(1)),
		ev(4,400,file,"PLAYER_DISCONNECT",pid(2),nil,nil),
	},200,false)
	w:=r.Sources[0].ConnectionWindows[0]
	if w.StartReason!="CONNECT"||w.EndReason!="DISCONNECT"||len(w.Lives)!=1||w.Lives[0].EndReason!="UNOBSERVED_LIFE_END" {
		t.Fatalf("killer was incorrectly treated as victim: %+v",w)
	}
	if has(w.Flags,"OBSERVATION_AFTER_RECORDED_DEATH_IN_SOURCE_ORDER"){t.Fatal("actor must not be flagged as dead")}
}

func TestSeparateSourcesNeverStitchedAndReconnectsAreExplicit(t *testing.T) {
	events:=[]Event{
		ev(7,700,"boot-A","PLAYER_DISCONNECT",pid(1),nil,nil),
		ev(2,200,"boot-A","PLAYER_CONNECT",pid(1),nil,nil),
		ev(1,100,"boot-A","PLAYER_HIT",nil,pid(2),pid(1)),
		ev(3,300,"boot-A","PLAYER_CONNECT",pid(1),nil,nil),
		ev(5,500,"boot-A","PLAYER_RESPAWN",pid(1),nil,nil),
		ev(6,600,"boot-A","PLAYER_DEATH",pid(1),nil,nil),
		ev(4,400,"boot-A","SUICIDE_ACTION",pid(1),nil,nil),
		ev(8,100,"boot-B","PLAYER_HIT",nil,pid(1),pid(2)),
	}
	r:=Reconstruct(1,events,200,true)
	if len(r.Sources)!=2||!r.WindowTruncated{t.Fatalf("source grouping/window wrong: %+v",r)}
	if r.Sources[0].SourceRef==r.Sources[1].SourceRef{t.Fatal("different boots must not be stitched")}
	a:=r.Sources[1].ConnectionWindows
	if len(a)!=3{t.Fatalf("expected missing-connect episode + explicit reconnect episodes: %+v",a)}
	if a[0].StartReason!="FIRST_OBSERVED"||a[0].EndReason!="REPEATED_CONNECT"{t.Fatalf("early gap not preserved: %+v",a[0])}
	if a[1].EndReason!="REPEATED_CONNECT"||!has(a[1].Flags,"REPEATED_CONNECT_WITHOUT_DISCONNECT"){t.Fatalf("missing disconnect not marked: %+v",a[1])}
	if a[2].StartReason!="CONNECT"||a[2].EndReason!="DISCONNECT"||len(a[2].Lives)!=2{
		t.Fatalf("explicit boundaries not applied correctly: %+v",a[2])
	}
	if !has(a[2].Observations[1].Notes,"SUICIDE_ACTION_NOT_PROOF_OF_DEATH"){t.Fatal("suicide action must not close life")}
	if a[2].Lives[0].EndReason!="RESPAWN_WITHOUT_RECORDED_DEATH"||
		a[2].Lives[1].EndReason!="RECORDED_DEATH" {t.Fatalf("life boundary classification wrong: %+v",a[2].Lives)}
	if r.Sources[0].ConnectionWindows[0].EndReason!="UNOBSERVED_END"{t.Fatal("source end cannot imply disconnect")}
}

func TestOnlyRequestedPlayerAndNullablePosition(t *testing.T){
	x,z:=4.0,8.0
	a:=ev(1,100,"source","PLAYER_HIT",nil,pid(2),pid(1))
	a.TargetX=&x;a.TargetZ=&z
	b:=ev(2,200,"source","PLAYER_HIT",nil,pid(2),pid(3))
	c:=ev(3,300,"source","PLAYER_HIT",nil,pid(2),pid(1))
	c.TargetX=&x // missing Z, not a complete observed position
	r:=Reconstruct(1,[]Event{a,b,c},200,false)
	w:=r.Sources[0].ConnectionWindows[0]
	if len(w.Observations)!=2||w.Observations[0].Position==nil||w.Observations[0].Position.X!=4||w.Observations[0].Position.Altitude!=nil||w.Observations[1].Position!=nil {
		t.Fatalf("leaked other player or invented position: %+v",w.Observations)
	}
	if len(Reconstruct(99,[]Event{a,b},200,false).Sources)!=0{t.Fatal("must not reconstruct unrelated player")}
}
