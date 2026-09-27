package caseintel

import (
 "math"
 "strings"
 "testing"
)

func ptrCoord(v float64)*float64{return &v}
func sample(id,offset int64,source,clock string)AdmissibilitySample{
 return AdmissibilitySample{EvidenceID:id,SourceID:source,SourceEndOffset:offset,
 LineSHA256:strings.Repeat("a",64),EventType:"PLAYER_HIT",ADMClock:clock,
 X:ptrCoord(100),Z:ptrCoord(200)}
}
func hasAdmissibilityBlocker(report AdmissibilityReport,code string)bool{
 for _,b:=range report.Blockers{if b==code{return true}}
 return false
}
func TestAdmissibilityNeverInfersTravelOrPunishment(t *testing.T){
 rows:=[]AdmissibilitySample{
  sample(1,10,"boot-a","23:59:59"),
  sample(2,50,"boot-a","23:59:59"),
  sample(3,200000,"boot-a","00:00:00"),
  sample(4,1,"boot-b","11:00:00"),
 }
 report,err:=AuditAdmissibility(rows,10,true)
 if err!=nil{t.Fatal(err)}
 if report.MovementDetectorStatus!="BLOCKED"||report.SafeSpeedPairs!=0||
 report.Enforcement!="DISABLED"||report.Coverage!="FILTERED_SOURCE_EVENTS_ONLY"||
 report.TimeBasis!=TimeBasis {t.Fatalf("unsafe or invented inference: %+v",report)}
 if report.SourceCount!=2||report.SameSecondAdjacent!=1||
 report.ClockDecreasesInSource!=1||report.CompleteCoordinatePairs!=4||
 report.ValidClockStrings!=4||report.WindowTruncated!=true{t.Fatalf("wrong audit: %+v",report)}
 for _,code:=range []string{"NO_DATED_HIGH_RESOLUTION_EVENT_TIME",
 "EVENT_TRIGGERED_POSITIONS_NOT_CONTINUOUS","NO_CROSS_SOURCE_STITCH",
 "BOUNDED_PAGE_EDGE","CLOCK_DECREASE_OR_DAY_ROLLOVER_UNRESOLVED",
 "FILTERED_EVENTS_CANNOT_PROVE_SOURCE_COMPLETENESS"}{
  if !hasAdmissibilityBlocker(report,code){t.Fatalf("missing %s",code)}
 }
 // Large gaps are expected after an event allowlist; never count them as lost lines.
 if report.InvalidSourceAddresses!=0||report.OffsetHashCollisions!=0{t.Fatalf("offset gap misclassified: %+v",report)}
}
func TestAdmissibilityClassifiesMalformedAndReplayedAddresses(t *testing.T){
 a:=sample(1,11,"boot-a","09:00:00")
 duplicate:=a;duplicate.EvidenceID=2
 collision:=a;collision.EvidenceID=3;collision.LineSHA256=strings.Repeat("b",64)
 invalid:=sample(4,-1,"boot-a","invalid");invalid.X=nil
 missing:=sample(5,12,"boot-a","");missing.X=nil;missing.Z=nil
 report,err:=AuditAdmissibility([]AdmissibilitySample{a,duplicate,collision,invalid,missing},5,false)
 if err!=nil{t.Fatal(err)}
 if report.DuplicateSourceAddresses!=1||report.OffsetHashCollisions!=1||
 report.InvalidSourceAddresses!=1||report.InvalidClockStrings!=2||
 report.PartialCoordinatePairs!=1||report.MissingCoordinatePairs!=1 {
  t.Fatalf("classification mismatch: %+v",report)
 }
 for _,code:=range []string{"INVALID_SOURCE_ADDRESS","SOURCE_OFFSET_HASH_COLLISION",
 "DUPLICATE_SOURCE_ADDRESS_IN_SAMPLE","ADM_CLOCK_MISSING_OR_INVALID",
 "PARTIAL_COORDINATE_PAIR"}{
  if !hasAdmissibilityBlocker(report,code){t.Fatalf("missing %s",code)}
 }
 if report.MovementDetectorStatus!="BLOCKED"||report.SafeSpeedPairs!=0{t.Fatal("bad telemetry unblocked movement")}
}
func TestAdmissibilityEmptyAndBoundsFailClosed(t *testing.T){
 empty,err:=AuditAdmissibility(nil,2,false)
 if err!=nil||empty.ObservationCount!=0||empty.SourceCount!=0||
 !hasAdmissibilityBlocker(empty,"NO_OBSERVED_EVENTS")||empty.MovementDetectorStatus!="BLOCKED"{
  t.Fatalf("empty sample must be explicitly unavailable: %+v, %v",empty,err)
 }
 for _,limit:=range []int{0,-1,501}{
  if _,err:=AuditAdmissibility(nil,limit,false);err==nil{t.Fatalf("accepted bad limit %d",limit)}
 }
 if _,err:=AuditAdmissibility([]AdmissibilitySample{sample(1,1,"a","01:00:00"),sample(2,2,"a","01:00:01")},1,false);err==nil{
  t.Fatal("accepted over-limit evidence")
 }
}

func TestAdmissibilityRoleCoordinatesNeverCombineAcrossPlayers(t *testing.T){
 a:=sample(1,1,"source","11:01:01")
 a.X=nil;a.Z=nil
 a.ActorX=ptrCoord(4) // incomplete actor
 a.TargetZ=ptrCoord(5) // incomplete target: cannot combine with actor X
 b:=sample(2,2,"source","11:01:02")
 b.ActorX=ptrCoord(6);b.ActorZ=ptrCoord(7)
 b.TargetX=ptrCoord(8);b.TargetZ=ptrCoord(9)
 report,err:=AuditAdmissibility([]AdmissibilitySample{a,b},2,false)
 if err!=nil{t.Fatal(err)}
 if report.CompleteCoordinatePairs!=1||report.MissingCoordinatePairs!=1||
 report.ActorCompleteCoordinatePairs!=1||report.ActorPartialCoordinatePairs!=1||
 report.TargetCompleteCoordinatePairs!=1||report.TargetPartialCoordinatePairs!=1||
 !hasAdmissibilityBlocker(report,"PARTIAL_COORDINATE_PAIR") {
  t.Fatalf("role-specific coordinate classification failed: %+v",report)
 }
 if report.SafeSpeedPairs!=0||report.MovementDetectorStatus!="BLOCKED"||
 report.Enforcement!="DISABLED"{t.Fatal("role positions cannot activate movement inference")}
}

func TestAdmissibilitySHAHexCaseDoesNotInventCollision(t *testing.T){
 a:=sample(1,10,"source","12:00:00")
 a.LineSHA256=strings.Repeat("ab",32)
 b:=a;b.EvidenceID=2;b.LineSHA256=strings.ToUpper(a.LineSHA256)
 report,err:=AuditAdmissibility([]AdmissibilitySample{a,b},2,false)
 if err!=nil{t.Fatal(err)}
 if report.DuplicateSourceAddresses!=1||report.OffsetHashCollisions!=0||
 hasAdmissibilityBlocker(report,"SOURCE_OFFSET_HASH_COLLISION") {
  t.Fatalf("SHA hex capitalization must not create a false source collision: %+v",report)
 }
}

func TestAdmissibilityNonFiniteCoordinatesAreUnusable(t *testing.T){
 a:=sample(1,10,"source","12:00:00")
 a.X=ptrCoord(math.NaN())
 a.ActorX=ptrCoord(math.Inf(1));a.ActorZ=ptrCoord(6)
 a.TargetX=ptrCoord(math.Inf(-1));a.TargetZ=ptrCoord(math.NaN())
 report,err:=AuditAdmissibility([]AdmissibilitySample{a},1,false)
 if err!=nil{t.Fatal(err)}
 if report.NonFiniteCoordinateValues!=4||
 report.CompleteCoordinatePairs!=0||report.PartialCoordinatePairs!=1||
 report.ActorCompleteCoordinatePairs!=0||report.ActorPartialCoordinatePairs!=1||
 report.TargetCompleteCoordinatePairs!=0||report.TargetMissingCoordinatePairs!=1||
 !hasAdmissibilityBlocker(report,"NON_FINITE_COORDINATE")||
 !hasAdmissibilityBlocker(report,"PARTIAL_COORDINATE_PAIR"){
  t.Fatalf("invalid numeric ADM axes counted as usable: %+v",report)
 }
 if report.SafeSpeedPairs!=0||report.MovementDetectorStatus!="BLOCKED"||report.Enforcement!="DISABLED"{
  t.Fatal("invalid coordinate evidence must never enable inference")
 }
}
