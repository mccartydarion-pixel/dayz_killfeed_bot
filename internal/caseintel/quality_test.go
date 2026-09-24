package caseintel

import "testing"

func qualityHas(blockers []string, want string) bool {
	for _, v := range blockers { if v==want { return true } }
	return false
}

func TestQualityNeverInfersSpeedFromClockOnlyEvidence(t *testing.T) {
	file:="source-A.ADM"
	a:=ev(1,100,file,"PLAYER_CONNECT",pid(4),nil,nil)
	b:=ev(2,200,file,"PLAYER_HIT",nil,pid(4),pid(7))
	c:=ev(3,300,file,"PLAYER_HIT",nil,pid(4),pid(7))
	a.ADMClock="09:43:19";b.ADMClock="09:43:19";c.ADMClock="09:43:20"
	out:=Reconstruct(4,[]Event{c,a,b},200,false)
	q:=out.Quality
	if q.TimeStatus!="CLOCK_ONLY_NO_TRUSTED_ELAPSED_TIME"||q.MovementDetectorStatus!="BLOCKED"||q.SafeSpeedPairs!=0 {
		t.Fatalf("must fail closed for clock-only telemetry: %+v",q)
	}
	if q.SourceCount!=1||q.ObservationCount!=3||q.SameClockAdjacentObservations!=1||q.UnobservedEndWindows!=1 {
		t.Fatalf("quality metrics incorrect: %+v",q)
	}
	if !qualityHas(q.Blockers,"NO_VERIFIED_ELAPSED_TIME")||!qualityHas(q.Blockers,"EVENT_TRIGGERED_POSITIONS_NOT_CONTINUOUS") ||
		!qualityHas(q.Blockers,"FILTERED_EVENTS_CANNOT_PROVE_SOURCE_COMPLETENESS") {
		t.Fatalf("missing provenance blocker: %+v",q.Blockers)
	}
}

func TestQualityRecognizesBoundaryAndClockAmbiguityWithoutCheatFinding(t *testing.T) {
	a:=ev(1,100,"boot-A.ADM","PLAYER_HIT",nil,pid(2),pid(7))
	b:=ev(2,200,"boot-A.ADM","PLAYER_KILL",nil,pid(2),pid(7))
	c:=ev(3,300,"boot-A.ADM","PLAYER_HIT",nil,pid(2),pid(7))
	d:=ev(4,100,"boot-B.ADM","PLAYER_DISCONNECT",pid(7),nil,nil)
	a.ADMClock="23:59:59";b.ADMClock="00:00:00";c.ADMClock="bad";d.ADMClock="01:00:00"
	out:=Reconstruct(7,[]Event{d,c,a,b},2,true)
	q:=out.Quality
	if q.SourceCount!=2||q.ObservationCount!=4||!q.WindowTruncated||
		q.ClockDecreasesInSource!=1||q.InvalidOrMissingADMClocks!=1||q.PostDeathSourceOrderWindows!=1{
		t.Fatalf("quality diagnostics wrong: %+v",q)
	}
	for _,want:=range []string{"BOUNDED_PAGE_EDGE","NO_CROSS_SOURCE_STITCH",
		"POST_DEATH_SOURCE_ORDER_AMBIGUITY","CLOCK_DECREASE_OR_DAY_ROLLOVER_UNRESOLVED"} {
		if !qualityHas(q.Blockers,want){t.Fatalf("missing %s: %+v",want,q.Blockers)}
	}
	if q.MovementDetectorStatus!="BLOCKED"||q.SafeSpeedPairs!=0{t.Fatal("uncertain evidence cannot enable timing detector")}
}
