package app

import (
 "testing"
 "time"
)

func TestCASECurrentSourceWitnessFailClosed(t *testing.T) {
 now:=time.Date(2026,9,27,22,0,0,0,time.UTC)
 selected:="selected-pseudonym"
 older:="historical-pseudonym"
 accepted:=true
 offset:=int64(456)
 ingested:=now.Add(-time.Minute)
 changed:=now.Add(-10*time.Minute)
 base:=caseSourceIntegrity{
  ServerID:1,GeneratedAt:now,WorkerAvailable:true,CollectorConfigured:true,
  SelectedSourceRef:&selected,AcceptedSourceRef:&selected,SelectedIsAccepted:&accepted,
  EvidenceObservationStatus:"RETAINED_EVENTS_OBSERVED",
  LatestEvidenceSourceRef:&selected,LatestEvidenceIngestedAt:&ingested,
  LatestEvidenceOffset:&offset,LastSourceChangeAt:&changed,
  MovementDetectorStatus:"BLOCKED",Enforcement:"DISABLED",
 }
 good:=caseCurrentSourceWitness(base)
 if good.Status!="CURRENT_SOURCE_REVIEW_CANDIDATE"||len(good.Reasons)!=1||
  good.Reasons[0]!="HUMAN_REVIEW_NON_ATOMIC_SNAPSHOTS_REQUIRED"||
  good.LatestEvidenceOffset!=offset {
  t.Fatalf("legitimate scoped observation must require human review: %+v",good)
 }
 tests:=[]struct{name string;mutate func(*caseSourceIntegrity)}{
  {"worker absent",func(x *caseSourceIntegrity){x.WorkerAvailable=false}},
  {"collector absent",func(x *caseSourceIntegrity){x.CollectorConfigured=false}},
  {"selected and accepted mismatch",func(x *caseSourceIntegrity){x.AcceptedSourceRef=&older}},
  {"historical latest",func(x *caseSourceIntegrity){x.LatestEvidenceSourceRef=&older;x.EvidenceObservationStatus="HISTORICAL_OR_OTHER_SOURCE_EVENTS"}},
  {"malformed status mismatch",func(x *caseSourceIntegrity){x.LatestEvidenceSourceRef=&older}},
  {"quiet boot",func(x *caseSourceIntegrity){x.EvidenceObservationStatus="NO_RETAINED_EVENTS"}},
  {"missing timestamp",func(x *caseSourceIntegrity){x.LatestEvidenceIngestedAt=nil}},
  {"missing offset",func(x *caseSourceIntegrity){x.LatestEvidenceOffset=nil}},
  {"negative offset",func(x *caseSourceIntegrity){n:=int64(-1);x.LatestEvidenceOffset=&n}},
  {"ingestion after capture",func(x *caseSourceIntegrity){v:=now.Add(time.Second);x.LatestEvidenceIngestedAt=&v}},
  {"old event predates boot",func(x *caseSourceIntegrity){v:=now.Add(-time.Hour);x.LatestEvidenceIngestedAt=&v}},
  {"zero server",func(x *caseSourceIntegrity){x.ServerID=0}},
 }
 for _,tt:=range tests{t.Run(tt.name,func(t *testing.T){
  x:=base;tt.mutate(&x);got:=caseCurrentSourceWitness(x)
  if got.Status!="NOT_OBSERVED"||len(got.Reasons)==0{t.Fatalf("unsafe accepted: %+v",got)}
 })}
}
