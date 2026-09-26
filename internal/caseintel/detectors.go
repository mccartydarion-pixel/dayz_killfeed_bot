package caseintel

import (
 "crypto/sha256"
 "encoding/hex"
 "fmt"
 "sort"
)

// Phase 2G.1 defines the evaluation contract, NOT an enabled cheat detector.
// A prerequisite failure yields a blocker, never a suspicion or a finding.
type DetectorDefinition struct {
 ID string `json:"id"`
 Version string `json:"version"`
 Name string `json:"name"`
 Mode string `json:"mode"`
 Prerequisites []string `json:"prerequisites"`
}
type PrerequisiteState struct {
 Name string `json:"name"`
 Status string `json:"status"`
 Reason string `json:"reason"`
}
type DetectorEvaluation struct {
 DetectorID string `json:"detectorId"`
 Version string `json:"version"`
 Status string `json:"status"`
 Blockers []PrerequisiteState `json:"blockers"`
 EvidenceIDs []int64 `json:"evidenceIds"`
 Fingerprint string `json:"fingerprint,omitempty"`
 Findings []any `json:"findings"`
 RiskScore *float64 `json:"riskScore"`
 Enforcement string `json:"enforcement"`
}

var detectorRegistry=[]DetectorDefinition{
 {ID:"CASE-MOV-001",Version:"0.1.0",Name:"Movement timing prerequisites",Mode:"BLOCKED",
 Prerequisites:[]string{"VERIFIED_EVENT_ELAPSED_TIME","VALIDATED_MOVEMENT_SAMPLES","SOURCE_CONTINUITY","EXCEPTION_MODEL"}},
}

// Registry returns independent copies: callers cannot mutate the global
// definitions or accidentally opt a detector into production.
func Registry() []DetectorDefinition {
 out:=make([]DetectorDefinition,len(detectorRegistry))
 for i,d:=range detectorRegistry {
  out[i]=d
  out[i].Prerequisites=append([]string(nil),d.Prerequisites...)
 }
 return out
}

// EvidenceFingerprint is stable regardless of input listing order, and binds
// a result to a tenant + game server + detector version + exact evidence IDs.
// It is NOT a substitute for an actual persisted, source-addressed evaluation.
func EvidenceFingerprint(guildID,serverID int64,detectorID,version string,ids []int64) (string,error) {
 if guildID<=0||serverID<=0||detectorID==""||version==""||len(ids)==0{return "",fmt.Errorf("invalid evaluation identity")}
 sorted:=append([]int64(nil),ids...)
 sort.Slice(sorted,func(i,j int)bool{return sorted[i]<sorted[j]})
 h:=sha256.New()
 fmt.Fprintf(h,"case-shadow-v1:%d:%d:%s:%s",guildID,serverID,detectorID,version)
 for i,id:=range sorted{
  if id<=0||(i>0&&id==sorted[i-1]){return "",fmt.Errorf("invalid or duplicate evidence ID")}
  fmt.Fprintf(h,":%d",id)
 }
 return hex.EncodeToString(h.Sum(nil)),nil
}

// EvaluatePrerequisites never manufactures a speed, accusation or finding.
// The current ADM clock is HH:MM:SS without a trusted date or subsecond
// resolution; ingestion timestamps must not be treated as game event times.
func EvaluatePrerequisites(def DetectorDefinition, quality QualityReport) DetectorEvaluation {
 out:=DetectorEvaluation{DetectorID:def.ID,Version:def.Version,Status:"BLOCKED",
  Blockers:make([]PrerequisiteState,0),EvidenceIDs:make([]int64,0),
  Findings:make([]any,0),RiskScore:nil,Enforcement:"DISABLED"}
 reason:=map[string]string{
  "VERIFIED_EVENT_ELAPSED_TIME":"ADM clocks are date-less seconds; ingestion is not gameplay time.",
  "VALIDATED_MOVEMENT_SAMPLES":"Event-triggered positions are not a continuous or validated movement trace.",
  "SOURCE_CONTINUITY":"Selected-event source coverage cannot prove complete sampling.",
  "EXCEPTION_MODEL":"Vehicle, respawn, teleport, admin and map-boundary exclusions are not validated.",
 }
 for _,p:=range def.Prerequisites {
  out.Blockers=append(out.Blockers,PrerequisiteState{Name:p,Status:"UNSATISFIED",Reason:reason[p]})
 }
 if quality.WindowTruncated {
  out.Blockers=append(out.Blockers,PrerequisiteState{Name:"BOUNDED_EVIDENCE_WINDOW",Status:"UNSATISFIED",Reason:"Older observations may lie outside this page."})
 }
 return out
}
