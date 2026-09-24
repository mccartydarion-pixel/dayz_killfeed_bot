package app

import (
 "testing"
 "time"

 "github.com/yourname/dayz-killfeed/internal/killfeed"
)

func TestCaseSourceIntegrityFailClosedAndPseudonymized(t *testing.T){
 now:=time.Date(2026,9,24,17,0,0,0,time.UTC)
 absent:=caseSourceSnapshot(1,now,killfeed.ADMSourceHealth{},killfeed.RuntimeDiagnosticSnapshot{},false)
 if absent.WorkerAvailable||absent.SourceState!="UNAVAILABLE"||absent.RemoteBytes!=nil||absent.CheckpointBytes!=nil||absent.LatestEvidenceIngestedAt!=nil||
  absent.ElapsedTimeTrusted||absent.DetectorsEnabled||absent.MovementDetectorStatus!="BLOCKED"||absent.Enforcement!="DISABLED" {
  t.Fatalf("must not assert health or continuity without worker: %+v",absent)
 }
 file:="dayzps/config/boot.ADM"
 old:=now.Add(-6*time.Minute)
 source:=killfeed.ADMSourceHealth{LastCycleAt:now,LastChangeAt:old,SelectedFile:file,AcceptedFile:file,OnlinePlayers:1}
 diag:=killfeed.RuntimeDiagnosticSnapshot{RemoteSize:124,CheckpointOffset:124,
  LastMetadataCheck:now,CheckpointLastSaved:now.Add(-time.Minute)}
 out:=caseSourceSnapshot(1,now,source,diag,true)
 if out.SourceState!=killfeed.ADMSourceLagging||out.SelectedSourceRef==nil||*out.SelectedSourceRef==file||
  out.AcceptedSourceRef==nil||*out.AcceptedSourceRef!=*out.SelectedSourceRef||
  out.SelectedIsAccepted==nil||!*out.SelectedIsAccepted||out.RemoteBytes==nil||*out.RemoteBytes!=124||
  out.CheckpointBytes==nil||*out.CheckpointBytes!=124||out.ElapsedTimeTrusted||out.MovementDetectorStatus!="BLOCKED"{
  t.Fatalf("source metadata health misreported: %+v",out)
 }
 if caseSourceRef("")!=nil||caseOptionalTime(time.Time{})!=nil {t.Fatal("unknown data must stay null")}
}
func TestCaseSourceIntegrityQuietIsNotEvidenceAbsence(t *testing.T){
 now:=time.Date(2026,9,24,17,0,0,0,time.UTC)
 state:=caseSourceSnapshot(1,now,killfeed.ADMSourceHealth{
  LastCycleAt:now,LastChangeAt:now.Add(-4*time.Minute),
  SelectedFile:"boot.ADM",OnlinePlayers:0,
 },killfeed.RuntimeDiagnosticSnapshot{},true)
 if state.SourceState!=killfeed.ADMQuiet||state.RemoteBytes!=nil||state.CheckpointBytes!=nil||
  state.EvidenceLines24h!=0||state.Coverage!="SELECTED_ADM_EVENTS_ONLY"{
  t.Fatalf("quiet/unknown state misleading: %+v",state)
 }
}
