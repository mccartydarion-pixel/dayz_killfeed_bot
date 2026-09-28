package caseintel

// InvestigationPolicy is an offline, no-caller policy for the eight-module
// review flow. Inputs must later be independently established from scoped,
// trusted sources; caller claims are never a production admission decision.
type Sensitivity string

const (
 SensitivityRelaxed Sensitivity = "RELAXED"
 SensitivityBalanced Sensitivity = "BALANCED"
 SensitivityStrict Sensitivity = "STRICT"
)

type InvestigationInput struct {
 ModuleID string
 Mode Sensitivity
 SourceCurrent bool
 PollingCaughtUp bool
 RequiredTelemetryPresent bool
 ModuleValidated bool
 EvidenceProvenanceVerified bool
 ExclusionsChecked bool
 DuplicateFree bool
 IndependentObservations int
}

type InvestigationDecision struct {
 Stage string `json:"stage"`
 Status string `json:"status"`
 Health string `json:"health"`
 Reasons []string `json:"reasons"`
 RequiredObservations int `json:"requiredObservations"`
 CanNotify bool `json:"canNotify"`
 ViolationEstablished bool `json:"violationEstablished"`
}

// AssessInvestigation cannot create a finding or send an alert. The chosen
// sensitivity affects only the count of separately validated observations
// needed for a future staff review candidate, never source requirements.
func AssessInvestigation(in InvestigationInput) InvestigationDecision {
 out:=InvestigationDecision{Stage:"OBSERVE",Status:"OBSERVATION_ONLY",Health:"HEALTHY",Reasons:[]string{},RequiredObservations:2}
 switch in.Mode {
 case "",SensitivityBalanced:
 case SensitivityRelaxed:out.RequiredObservations=3
 case SensitivityStrict:out.RequiredObservations=1
 default:out.Reasons=append(out.Reasons,"INVALID_SENSITIVITY")
 }
 known:=false
 for _,d:=range ClientCatalog(){if d.ID==in.ModuleID{known=true;break}}
 if !known {out.Reasons=append(out.Reasons,"UNKNOWN_MODULE")}
 if !in.SourceCurrent {out.Reasons=append(out.Reasons,"SOURCE_STALE")}
 if !in.PollingCaughtUp {out.Reasons=append(out.Reasons,"POLLING_BEHIND")}
 if !in.RequiredTelemetryPresent {out.Reasons=append(out.Reasons,"REQUIRED_TELEMETRY_MISSING")}
 if !in.SourceCurrent||!in.PollingCaughtUp||!in.RequiredTelemetryPresent {
  out.Stage="VALIDATE";out.Status="SUSPENDED";out.Health="DEGRADED"
  return out
 }
 if !in.ModuleValidated {out.Reasons=append(out.Reasons,"MODULE_NOT_VALIDATED")}
 if !in.EvidenceProvenanceVerified {out.Reasons=append(out.Reasons,"PROVENANCE_UNVERIFIED")}
 if !in.ExclusionsChecked {out.Reasons=append(out.Reasons,"EXCLUSIONS_UNCHECKED")}
 if !in.DuplicateFree {out.Reasons=append(out.Reasons,"DUPLICATE_EVIDENCE")}
 if in.IndependentObservations<out.RequiredObservations {out.Reasons=append(out.Reasons,"INSUFFICIENT_CORROBORATION")}
 if len(out.Reasons)>0 {out.Stage="VALIDATE";return out}
 out.Stage="VALIDATE";out.Status="REVIEW_CANDIDATE"
 // NOTIFY is a separate authenticated, default-off capability. Even a
 // candidate is neutral and cannot establish a violation or send a message.
 return out
}
