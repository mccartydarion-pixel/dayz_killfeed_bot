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
 defs:=[]caseintel.DetectorDefinition{{ID:"CASE-LOGIN-001",Mode:"ACTIVE"},
  {ID:"CASE-PC-XBOX-001",Mode:"BLOCKED"}}
 got:=caseAssessDetectorCatalogHealth(defs,caseSourceIntegrity{})
 if len(got)!=2||got[0].State!="ERROR"||!got[0].ConclusionsSuspended||
  got[1].State!="ERROR"||!got[1].ConclusionsSuspended {
  t.Fatalf("unexpected mode silently activated: %+v",got)
 }
}

func TestCoreEightRequiredTelemetryExplainsWithoutPromoting(t *testing.T) {
 now:=time.Date(2026,9,30,0,0,0,0,time.UTC)
 poll:=now.Add(-time.Minute)
 ref:="source-ref"
 accepted:=true
 s:=caseSourceIntegrity{GeneratedAt:now,WorkerAvailable:true,SourceState:killfeed.ADMHealthy,
  LastPollAt:&poll,CollectorConfigured:true,SelectedSourceRef:&ref,AcceptedSourceRef:&ref,SelectedIsAccepted:&accepted}
 defs:=caseintel.ClientCatalog()
 before:=caseAssessDetectorCatalogHealth(defs,s)
 got:=caseAssessDetectorCatalogHealth(defs,s)
 caseAttachRequiredTelemetry(got,s,false)
 status:=func(h caseDetectorCatalogHealth,kind caseintel.TelemetryKind)string{
  for _,f:=range h.RequiredTelemetry{if f.Kind==string(kind){return f.Status}}
  return ""
 }
 for i,h:=range got {
  if h.State!=before[i].State||len(h.Reasons)!=len(before[i].Reasons) {t.Fatalf("telemetry changed health: %+v",h)}
  want:=caseintel.RequiredTelemetry(h.ModuleID)
  if len(want)==0||len(h.RequiredTelemetry)!=len(want) {t.Fatalf("%s telemetry contract mismatch: %+v",h.ModuleID,h.RequiredTelemetry)}
  for j,f:=range h.RequiredTelemetry {
   if f.Kind!=string(want[j])||f.Reason=="" {t.Fatalf("%s feed %d: %+v",h.ModuleID,j,f)}
   if f.Status=="CURRENT"&&f.Kind!=string(caseintel.TelemetryADMEvents) {t.Fatalf("%s: non-ADM feed reported current: %+v",h.ModuleID,f)}
  }
  // Every module still lacks at least one feed, so none can look operational.
  missing:=false
  for _,f:=range h.RequiredTelemetry{missing=missing||f.Status!="CURRENT"}
  if !missing {t.Fatalf("%s shows all telemetry current",h.ModuleID)}
 }
 byID:=map[string]caseDetectorCatalogHealth{}
 for _,h:=range got{byID[h.ModuleID]=h}
 if status(byID["CASE-TELEPORT-001"],caseintel.TelemetryADMEvents)!="CURRENT" {t.Fatalf("fresh ADM not current")}
 if status(byID["CASE-PC-XBOX-001"],caseintel.TelemetryPlatformAttestation)!="UNSUPPORTED" {t.Fatalf("platform attestation must be unsupported")}
 if status(byID["CASE-DUPE-001"],caseintel.TelemetryInventoryItems)!="UNSUPPORTED" {t.Fatalf("inventory must be unsupported")}
 if status(byID["CASE-BASE-001"],caseintel.TelemetryBuildActions)!="NOT_CONFIGURED" {t.Fatalf("build collection off")}
 caseAttachRequiredTelemetry(got,s,true)
 if status(got[0],caseintel.TelemetryBuildActions)!="UNVERIFIED" {t.Fatalf("enabled build collection claimed verified: %+v",got[0])}

 old:=now.Add(-10*time.Minute)
 s.LastPollAt=&old
 caseAttachRequiredTelemetry(got,s,false)
 if status(got[0],caseintel.TelemetryADMEvents)!="STALE" {t.Fatalf("stale poll: %+v",got[0])}
 s.WorkerAvailable=false
 caseAttachRequiredTelemetry(got,s,false)
 if status(got[0],caseintel.TelemetryADMEvents)!="UNAVAILABLE" {t.Fatalf("no worker: %+v",got[0])}
}
