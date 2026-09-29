package app

import (
 "time"

 "github.com/yourname/dayz-killfeed/internal/caseintel"
 "github.com/yourname/dayz-killfeed/internal/killfeed"
)

// This is a read-only availability projection for the protected Core Eight
// catalog. There is no owner enablement, detector evaluation, or delivery
// implied by a healthy ADM poll.
type caseDetectorCatalogHealth struct {
 ModuleID string `json:"moduleId"`
 State string `json:"state"`
 Reasons []string `json:"reasons"`
 LastSuccessfulEvaluationAt *time.Time `json:"lastSuccessfulEvaluationAt"`
 ConclusionsSuspended bool `json:"conclusionsSuspended"`
}

func caseAssessDetectorCatalogHealth(defs []caseintel.DetectorDefinition, s caseSourceIntegrity) []caseDetectorCatalogHealth {
 out:=make([]caseDetectorCatalogHealth,0,len(defs))
 for _,def:=range defs {
  item:=caseDetectorCatalogHealth{ModuleID:def.ID,State:"INSUFFICIENT_EVIDENCE",
   Reasons:[]string{},ConclusionsSuspended:true}
  // No trusted client-platform attestation exists in the ADM collector.
  if def.ID=="CASE-PC-XBOX-001" {
   item.State="UNSUPPORTED"
   item.Reasons=append(item.Reasons,"TRUSTED_PLATFORM_METADATA_UNAVAILABLE")
  } else if !s.WorkerAvailable {
   item.State="DEGRADED"
   item.Reasons=append(item.Reasons,"WORKER_UNAVAILABLE")
  } else if s.SourceState!=killfeed.ADMHealthy&&s.SourceState!=killfeed.ADMQuiet {
   item.State="DEGRADED"
   item.Reasons=append(item.Reasons,"SOURCE_NOT_CURRENT")
  } else if s.GeneratedAt.IsZero()||s.LastPollAt==nil||
   s.LastPollAt.After(s.GeneratedAt)||s.GeneratedAt.Sub(*s.LastPollAt)>2*time.Minute {
   item.State="DEGRADED"
   item.Reasons=append(item.Reasons,"POLL_FRESHNESS_UNVERIFIED")
  } else if !s.CollectorConfigured {
   item.Reasons=append(item.Reasons,"EVIDENCE_COLLECTOR_NOT_CONFIGURED")
  } else if s.SelectedSourceRef==nil||s.AcceptedSourceRef==nil||
   s.SelectedIsAccepted==nil||!*s.SelectedIsAccepted||
   *s.SelectedSourceRef!=*s.AcceptedSourceRef {
   item.Reasons=append(item.Reasons,"SOURCE_IDENTITY_UNVERIFIED")
  } else {
   item.Reasons=append(item.Reasons,"MODULE_TELEMETRY_UNVALIDATED")
  }
  if def.Mode!="BLOCKED" {
   // A catalog change cannot silently promote this protected view.
   item.State="ERROR"
   item.Reasons=append(item.Reasons,"UNEXPECTED_CATALOG_MODE")
  }
  item.Reasons=append(item.Reasons,"MODULE_NOT_RELEASED")
  out=append(out,item)
 }
 return out
}
