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

func TestCASEEvidenceObservationStatusIsNotAHealthOrCheatVerdict(t *testing.T) {
 now:=time.Date(2026,9,27,12,0,0,0,time.UTC)
 source:=caseSourceRef("source-a.ADM")
 tests:=[]struct{
  name string
  worker,collector bool
  source *string
  latest *time.Time
  count int64
  want string
 }{
  {"worker absent despite historical rows",false,true,source,&now,5,"WORKER_UNAVAILABLE"},
  {"collector off despite historical rows",true,false,source,&now,5,"COLLECTOR_NOT_CONFIGURED"},
  {"source missing despite retained rows",true,true,nil,&now,5,"SOURCE_UNVERIFIED"},
  {"quiet empty sample",true,true,source,nil,0,"NO_RETAINED_EVENTS"},
  {"old events still observed",true,true,source,&now,0,"RETAINED_EVENTS_OBSERVED"},
  {"recent count but latest missing",true,true,source,nil,1,"RETAINED_EVENTS_OBSERVED"},
 }
 for _,tt:=range tests {
  t.Run(tt.name,func(t *testing.T){
   got:=caseEvidenceObservationStatus(tt.worker,tt.collector,tt.source,tt.latest,tt.count)
   if got!=tt.want {t.Fatalf("got %q want %q",got,tt.want)}
  })
 }
}
