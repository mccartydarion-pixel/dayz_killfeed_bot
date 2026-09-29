package app

import (
 "testing"
 "time"

 "github.com/yourname/dayz-killfeed/internal/caseintel"
 "github.com/yourname/dayz-killfeed/internal/killfeed"
)

func TestCoreEightCatalogHealthNeverInfersActiveFromPolling(t *testing.T) {
 now:=time.Date(2026,9,29,0,0,0,0,time.UTC)
 poll:=now.Add(-time.Minute)
 ref:="source-ref"
 accepted:=true
 s:=caseSourceIntegrity{GeneratedAt:now,WorkerAvailable:true,SourceState:killfeed.ADMHealthy,
  LastPollAt:&poll,CollectorConfigured:true,SelectedSourceRef:&ref,
  AcceptedSourceRef:&ref,SelectedIsAccepted:&accepted}
 defs:=caseintel.ClientCatalog()
 got:=caseAssessDetectorCatalogHealth(defs,s)
 if len(got)!=8 {t.Fatalf("expected eight health entries, got %d",len(got))}
 for i,h:=range got {
  if h.ModuleID!=defs[i].ID||!h.ConclusionsSuspended||
   h.LastSuccessfulEvaluationAt!=nil||h.State=="ACTIVE"||h.State=="DISABLED" {
   t.Fatalf("unverified detector presented as operational: %+v",h)
  }
  if h.ModuleID=="CASE-PC-XBOX-001" {
   if h.State!="UNSUPPORTED" {t.Fatalf("Xbox PC detection must be unsupported: %+v",h)}
  } else if h.State!="INSUFFICIENT_EVIDENCE" {
   t.Fatalf("source poll cannot validate module: %+v",h)
  }
 }
 s.SelectedIsAccepted=nil
 for _,h:=range caseAssessDetectorCatalogHealth(defs,s) {
  if h.ModuleID!="CASE-PC-XBOX-001"&&h.State!="INSUFFICIENT_EVIDENCE" {t.Fatalf("unverified source identity: %+v",h)}
 }
 s.SelectedIsAccepted=&accepted
 s.CollectorConfigured=false
 for _,h:=range caseAssessDetectorCatalogHealth(defs,s) {
  if h.ModuleID!="CASE-PC-XBOX-001"&&h.State!="INSUFFICIENT_EVIDENCE" {t.Fatalf("missing collector: %+v",h)}
 }
 s.CollectorConfigured=true
 s.LastPollAt=nil
 for _,h:=range caseAssessDetectorCatalogHealth(defs,s) {
  if h.ModuleID!="CASE-PC-XBOX-001"&&h.State!="DEGRADED" {t.Fatalf("missing poll should degrade: %+v",h)}
 }
 s.WorkerAvailable=false
 for _,h:=range caseAssessDetectorCatalogHealth(defs,s) {
  if h.ModuleID!="CASE-PC-XBOX-001"&&h.State!="DEGRADED" {t.Fatalf("missing worker should degrade: %+v",h)}
 }
}

func TestCatalogHealthFailsClosedForUnexpectedReleaseMode(t *testing.T) {
 defs:=[]caseintel.DetectorDefinition{{ID:"CASE-LOGIN-001",Mode:"ACTIVE"}}
 got:=caseAssessDetectorCatalogHealth(defs,caseSourceIntegrity{})
 if len(got)!=1||got[0].State!="ERROR"||!got[0].ConclusionsSuspended {
  t.Fatalf("unexpected mode silently activated: %+v",got)
 }
}
