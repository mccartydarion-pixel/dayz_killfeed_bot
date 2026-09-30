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
 RequiredTelemetry []caseTelemetryStatus `json:"requiredTelemetry"`
}

// caseTelemetryStatus explains, per required feed, why a module cannot run.
// CURRENT is only ever reported for the ADM source, from the live worker
// snapshot; every other feed is labelled from what the collector can
// actually provide today. No status here can promote a module.
type caseTelemetryStatus struct {
 Kind string `json:"kind"`
 Status string `json:"status"`
 Reason string `json:"reason"`
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
  if (def.ID=="CASE-PC-XBOX-001" && def.Mode!="UNSUPPORTED") ||
   (def.ID!="CASE-PC-XBOX-001" && def.Mode!="BLOCKED") {
   // A catalog change cannot silently promote this protected view.
   item.State="ERROR"
   item.Reasons=append(item.Reasons,"UNEXPECTED_CATALOG_MODE")
  }
  item.Reasons=append(item.Reasons,"MODULE_NOT_RELEASED")
  out=append(out,item)
 }
 return out
}

// caseAttachRequiredTelemetry fills RequiredTelemetry from the Core Eight
// engine's per-module data contract. It never changes State or Reasons.
func caseAttachRequiredTelemetry(health []caseDetectorCatalogHealth, s caseSourceIntegrity, buildEvidenceEnabled bool) {
 for i:=range health {
  kinds:=caseintel.RequiredTelemetry(health[i].ModuleID)
  health[i].RequiredTelemetry=make([]caseTelemetryStatus,0,len(kinds))
  for _,k:=range kinds {
   health[i].RequiredTelemetry=append(health[i].RequiredTelemetry,caseFeedStatus(k,s,buildEvidenceEnabled))
  }
 }
}

func caseFeedStatus(kind caseintel.TelemetryKind, s caseSourceIntegrity, buildEvidenceEnabled bool) caseTelemetryStatus {
 st:=func(status,reason string)caseTelemetryStatus{return caseTelemetryStatus{Kind:string(kind),Status:status,Reason:reason}}
 switch kind {
 case caseintel.TelemetryADMEvents:
  switch {
  case !s.WorkerAvailable:
   return st("UNAVAILABLE","No running ADM worker for this server.")
  case s.SourceState!=killfeed.ADMHealthy&&s.SourceState!=killfeed.ADMQuiet:
   return st("STALE","The selected ADM source is not current.")
  case s.GeneratedAt.IsZero()||s.LastPollAt==nil||s.LastPollAt.After(s.GeneratedAt)||s.GeneratedAt.Sub(*s.LastPollAt)>2*time.Minute:
   return st("STALE","The last ADM poll is older than two minutes or unverified.")
  }
  return st("CURRENT","ADM polling is current. ADM clocks are date-less, so event times are not trusted.")
 case caseintel.TelemetryBuildActions:
  if !buildEvidenceEnabled {
   return st("NOT_CONFIGURED","Build-action evidence collection is off for this server.")
  }
  return st("UNVERIFIED","Build actions are collected, but no real ADM build line has been verified.")
 case caseintel.TelemetryPositionSamples:
  return st("UNVERIFIED","Only event-triggered positions without trusted event times are available.")
 case caseintel.TelemetrySessionEvents:
  return st("UNVERIFIED","Connect and disconnect lines are retained; event times are derived from the boot stamp and learned UTC offset for the staff shadow read only, not validated for detection.")
 case caseintel.TelemetryBaseRegistry:
  return st("UNAVAILABLE","Base registration is not released.")
 case caseintel.TelemetryRestartSchedule:
  return st("UNAVAILABLE","Restarts are derived from boot boundaries for the staff shadow read only; no validated restart schedule is connected.")
 case caseintel.TelemetryTerrainModel,caseintel.TelemetryStructureGeometry:
  return st("UNAVAILABLE","No verified terrain or structure model for this map.")
 case caseintel.TelemetryVehicleState:
  return st("UNAVAILABLE","Vehicle state is not available from the ADM log.")
 case caseintel.TelemetryInventoryItems:
  return st("UNSUPPORTED","The ADM log has no item identity or inventory transactions.")
 case caseintel.TelemetryPlatformAttestation:
  return st("UNSUPPORTED","Console servers expose no trusted client-platform attestation.")
 }
 return st("UNAVAILABLE","Unknown telemetry feed.")
}
