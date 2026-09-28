package app

import "time"

// caseSourceWitness is a pure operator-review aid for issue #114. It does
// not change the integrity endpoint, query production, accept release gates,
// create findings, evaluate a detector or send an alert. A candidate requires
// independent human review of the protected capture and its sampling limits.
type caseSourceWitness struct {
 Status string
 Reasons []string
 SelectedSourceRef string
 LatestEvidenceSourceRef string
 LatestEvidenceOffset int64
 LatestEvidenceIngestedAt time.Time
}

func caseCurrentSourceWitness(s caseSourceIntegrity) caseSourceWitness {
 out:=caseSourceWitness{Status:"NOT_OBSERVED"}
 reason:=func(code string){out.Reasons=append(out.Reasons,code)}
 if s.ServerID<=0||s.GeneratedAt.IsZero() {reason("INVALID_CAPTURE");return out}
 if !s.WorkerAvailable {reason("WORKER_UNAVAILABLE");return out}
 if !s.CollectorConfigured {reason("COLLECTOR_NOT_CONFIGURED");return out}
 if s.SelectedSourceRef==nil||s.AcceptedSourceRef==nil||
  s.SelectedIsAccepted==nil||!*s.SelectedIsAccepted||
  *s.SelectedSourceRef==""||*s.SelectedSourceRef!=*s.AcceptedSourceRef {
  reason("SELECTED_SOURCE_UNVERIFIED");return out
 }
 out.SelectedSourceRef=*s.SelectedSourceRef
 if s.EvidenceObservationStatus!="RETAINED_EVENTS_OBSERVED" {
  reason("NO_MATCHING_CURRENT_RETAINED_EVENT");return out
 }
 if s.LatestEvidenceSourceRef==nil||*s.LatestEvidenceSourceRef!=*s.SelectedSourceRef {
  reason("LATEST_EVENT_SOURCE_MISMATCH");return out
 }
 if s.LatestEvidenceIngestedAt==nil||s.LatestEvidenceIngestedAt.IsZero()||
  s.LatestEvidenceOffset==nil||*s.LatestEvidenceOffset<0 {
  reason("LATEST_EVENT_ADDRESS_INCOMPLETE");return out
 }
 out.LatestEvidenceSourceRef=*s.LatestEvidenceSourceRef
 out.LatestEvidenceOffset=*s.LatestEvidenceOffset
 out.LatestEvidenceIngestedAt=s.LatestEvidenceIngestedAt.UTC()
 if out.LatestEvidenceIngestedAt.After(s.GeneratedAt) {
  reason("CAPTURE_CLOCK_INCONSISTENT");return out
 }
 // A retained row from a previous observation window cannot independently
 // satisfy current-boot acceptance when an observed source change is newer.
 if s.LastSourceChangeAt!=nil && out.LatestEvidenceIngestedAt.Before(*s.LastSourceChangeAt) {
  reason("EVENT_PREDATES_OBSERVED_SOURCE_CHANGE");return out
 }
 // Source metadata and retained evidence are separate non-atomic snapshots.
 // This label is only a review candidate, never gate PASS or proof of
 // continuous ADM completeness, detector validity or delivery.
 out.Status="CURRENT_SOURCE_REVIEW_CANDIDATE"
 reason("HUMAN_REVIEW_NON_ATOMIC_SNAPSHOTS_REQUIRED")
 return out
}
