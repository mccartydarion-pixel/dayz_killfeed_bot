package caseintel

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// The Core Eight engine is an OFFLINE evaluator. Every detector runs the same
// Observe -> Correlate -> Validate stages over caller-supplied, scoped inputs
// and ends at a neutral staff-review candidate. It has no production caller,
// sends nothing and cannot enforce. NOTIFY remains the existing default-off
// casealert/caseoutbox path, gated on a released catalog mode.

// EvidenceTier keeps the three Phase 5 levels apart. Detectors can only emit
// OBSERVED or SUSPICIOUS; STAFF_CONFIRMED requires ConfirmByStaff.
type EvidenceTier string

const (
	TierObserved       EvidenceTier = "OBSERVED"
	TierSuspicious     EvidenceTier = "SUSPICIOUS"
	TierStaffConfirmed EvidenceTier = "STAFF_CONFIRMED"
)

type Core8Scope struct{ GuildID, InstallationID, ServerID int64 }

func (s Core8Scope) valid() bool { return s.GuildID > 0 && s.InstallationID > 0 && s.ServerID > 0 }

type Coordinates struct {
	X        float64  `json:"x"`
	Z        float64  `json:"z"`
	Altitude *float64 `json:"altitude,omitempty"`
}

// Finding is the evidence record every Core Eight detector preserves. Evidence
// IDs reference retained, source-addressed rows; raw paths are never stored.
type Finding struct {
	DetectorID           string        `json:"detectorId"`
	DetectorVersion      string        `json:"detectorVersion"`
	Scope                Core8Scope    `json:"scope"`
	PlayerID             int64         `json:"playerId"`
	PlayerName           string        `json:"playerName,omitempty"`
	EvidenceIDs          []int64       `json:"evidenceIds"`
	EventAt              time.Time     `json:"eventAt"`
	ObservedAt           time.Time     `json:"observedAt"`
	Coordinates          []Coordinates `json:"coordinates,omitempty"`
	Behavior             string        `json:"behavior"`
	Explanation          string        `json:"explanation"`
	EvidenceCompleteness string        `json:"evidenceCompleteness"`
	MissingEvidence      []string      `json:"missingEvidence"`
	ExclusionsChecked    []string      `json:"exclusionsChecked"`
	RelatedIncidentKeys  []string      `json:"relatedIncidentKeys,omitempty"`
	AffectedBaseID       string        `json:"affectedBaseId,omitempty"`
	Tier                 EvidenceTier  `json:"tier"`
	InvestigationStatus  string        `json:"investigationStatus"`
	ConfirmedBy          string        `json:"confirmedBy,omitempty"`
	IncidentKey          string        `json:"incidentKey"`

	// keyEvidenceIDs, when set, is the stable qualifying prefix of a growing
	// run. A later poll that extends the run keeps the same incident key.
	keyEvidenceIDs []int64
}

// Core8Result is the outcome of one detector over one player's window.
type Core8Result struct {
	DetectorID           string         `json:"detectorId"`
	Stage                string         `json:"stage"`
	Status               string         `json:"status"`
	Health               DetectorHealth `json:"health"`
	Reasons              []string       `json:"reasons"`
	Findings             []Finding      `json:"findings"`
	Exclusions           map[string]int `json:"exclusions"`
	RequiredObservations int            `json:"requiredObservations"`
	CanNotify            bool           `json:"canNotify"`
	ViolationEstablished bool           `json:"violationEstablished"`
	Enforcement          string         `json:"enforcement"`
}

// EvalContext binds one evaluation to an installation, owner configuration
// and the telemetry health that gates it.
type EvalContext struct {
	Scope      Core8Scope
	PlayerID   int64
	PlayerName string
	Enabled    bool
	Mode       Sensitivity
	Thresholds *ValidatedThresholds
	Telemetry  TelemetrySnapshot
	Params     Core8Params
}

// Core8Params are physical and geometric limits, distinct from the owner's
// sensitivity (which only changes how many independent observations are
// needed). DefaultCore8Params are conservative staging values, not validated
// production thresholds.
type Core8Params struct {
	MaxSampleLag            time.Duration // event time -> collector observation
	TeleportMinDistance     float64       // metres
	TeleportMaxFootSpeed    float64       // metres per second
	TeleportMaxGap          time.Duration // wider gaps are unobserved movement, not evidence
	ElevationMinOffset      float64       // metres above terrain for Skywalk
	UndermapMinDepth        float64       // metres below terrain for Undermap
	ElevationMinConsecutive int
	ElevationMinDuration    time.Duration
	NoClipMaxInterval       time.Duration
	NoClipMinPenetration    float64
	BaseMinRadius           float64
	BaseMaxRadius           float64
	LoginReconnectGap       time.Duration
	LoginBurstWindow        time.Duration
	LoginMinReconnects      int
	RestartGrace            time.Duration
	DupeCorrelationWindow   time.Duration
}

func DefaultCore8Params() Core8Params {
	return Core8Params{MaxSampleLag: 2 * time.Minute, TeleportMinDistance: 250, TeleportMaxFootSpeed: 12,
		TeleportMaxGap: 2 * time.Minute, ElevationMinOffset: 15, UndermapMinDepth: 3,
		ElevationMinConsecutive: 3, ElevationMinDuration: 10 * time.Second,
		NoClipMaxInterval: 2 * time.Second, NoClipMinPenetration: 0.75,
		BaseMinRadius: 10, BaseMaxRadius: 150,
		LoginReconnectGap: 90 * time.Second, LoginBurstWindow: 10 * time.Minute, LoginMinReconnects: 4,
		RestartGrace: 10 * time.Minute, DupeCorrelationWindow: 5 * time.Minute}
}

// RestartWindow is a verified server restart interval from the scheduler or
// the host API; an unexplained ADM gap is not a restart.
type RestartWindow struct{ Start, End time.Time }

func withinRestart(windows []RestartWindow, grace time.Duration, times ...time.Time) bool {
	for _, w := range windows {
		for _, t := range times {
			if !t.Before(w.Start.Add(-grace)) && !t.After(w.End.Add(grace)) {
				return true
			}
		}
	}
	return false
}

func spansRestart(windows []RestartWindow, grace time.Duration, from, to time.Time) bool {
	for _, w := range windows {
		if !to.Before(w.Start.Add(-grace)) && !from.After(w.End.Add(grace)) {
			return true
		}
	}
	return withinRestart(windows, grace, from, to)
}

// TimedEvidence is the minimum timestamp contract for any input sample.
// EventAt must be a trusted game/server event time; ObservedAt is when the
// collector retained it. Ingestion time is never substituted for EventAt.
type TimedEvidence struct {
	EvidenceID  int64
	PlayerID    int64
	EventAt     time.Time
	TimeTrusted bool
	ObservedAt  time.Time
}

// sampleProblem returns the exclusion code for an unusable sample, or "".
func (e TimedEvidence) sampleProblem(ctx EvalContext) string {
	switch {
	case e.EvidenceID <= 0:
		return "MISSING_EVIDENCE_ID"
	case e.PlayerID != ctx.PlayerID:
		return "PLAYER_MISMATCH"
	case !e.TimeTrusted || e.EventAt.IsZero():
		return "EVENT_TIME_UNTRUSTED"
	case e.ObservedAt.IsZero() || e.ObservedAt.Before(e.EventAt):
		return "OBSERVATION_TIME_INVALID"
	case ctx.Params.MaxSampleLag <= 0 || e.ObservedAt.Sub(e.EventAt) > ctx.Params.MaxSampleLag:
		return "SAMPLE_STALE"
	case !ctx.Telemetry.Now.IsZero() && e.EventAt.After(ctx.Telemetry.Now):
		return "EVENT_IN_FUTURE"
	}
	return ""
}

// IncidentKey is stable across polling retries, reconnects, restarts and
// journal recovery: it depends only on scope, detector version, player and the
// sorted retained evidence IDs, never on wall-clock or processing order.
func IncidentKey(scope Core8Scope, detectorID, version string, playerID int64, evidenceIDs []int64) (string, error) {
	if !scope.valid() || detectorID == "" || version == "" || playerID <= 0 || len(evidenceIDs) == 0 {
		return "", errors.New("invalid incident identity")
	}
	sorted := append([]int64(nil), evidenceIDs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	h := sha256.New()
	fmt.Fprintf(h, "case-core8-incident-v1:%d:%d:%d:%s:%s:%d", scope.GuildID, scope.InstallationID, scope.ServerID, detectorID, version, playerID)
	for i, id := range sorted {
		if id <= 0 || (i > 0 && id == sorted[i-1]) {
			return "", errors.New("invalid or duplicate evidence ID")
		}
		fmt.Fprintf(h, ":%d", id)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ConfirmByStaff is the only way to reach STAFF_CONFIRMED. It returns a copy
// and never triggers enforcement; sanctions stay a separate human action.
func ConfirmByStaff(f Finding, reviewerID string) (Finding, error) {
	if strings.TrimSpace(reviewerID) == "" {
		return f, errors.New("staff reviewer required")
	}
	if f.Tier != TierSuspicious || f.IncidentKey == "" {
		return f, errors.New("only a suspicious incident can be staff-confirmed")
	}
	out := f
	out.EvidenceIDs = append([]int64(nil), f.EvidenceIDs...)
	out.Tier, out.ConfirmedBy, out.InvestigationStatus = TierStaffConfirmed, reviewerID, "STAFF_CONFIRMED"
	return out, nil
}

// evaluation collects Observe/Correlate output for finalize.
type evaluation struct {
	ctx        EvalContext
	def        DetectorDefinition
	candidates []Finding
	exclusions map[string]int
	reasons    []string
}

func newEvaluation(ctx EvalContext, moduleID string) (*evaluation, *Core8Result) {
	ev := &evaluation{ctx: ctx, exclusions: map[string]int{}}
	for _, d := range ClientCatalog() {
		if d.ID == moduleID {
			ev.def = d
		}
	}
	res := &Core8Result{DetectorID: moduleID, Stage: "OBSERVE", Status: "NO_OBSERVATION", Reasons: []string{},
		Findings: []Finding{}, Exclusions: ev.exclusions, Enforcement: "DISABLED"}
	res.Health = AssessCore8Health(moduleID, ctx.Enabled, ctx.Telemetry)
	switch {
	case ev.def.ID == "":
		res.Status = "UNSUPPORTED"
		res.Reasons = append(res.Reasons, "UNKNOWN_MODULE")
	case !ctx.Scope.valid() || ctx.PlayerID <= 0:
		res.Stage, res.Status = "VALIDATE", "ERROR"
		res.Reasons = append(res.Reasons, "INVALID_INSTALLATION_SCOPE")
	case res.Health.State == "DISABLED" || res.Health.State == "UNSUPPORTED" || res.Health.State == "ERROR":
		res.Status = res.Health.State
		res.Reasons = append(res.Reasons, res.Health.Reasons...)
	case res.Health.State == "DEGRADED":
		// Stale or lagging telemetry suspends every dependent conclusion.
		res.Stage, res.Status = "VALIDATE", "SUSPENDED"
		res.Reasons = append(res.Reasons, res.Health.Reasons...)
	case hasString(res.Health.Reasons, "REQUIRED_TELEMETRY_MISSING"):
		res.Stage, res.Status = "VALIDATE", "INSUFFICIENT_EVIDENCE"
		res.Reasons = append(res.Reasons, res.Health.Reasons...)
	default:
		return ev, nil
	}
	sort.Strings(res.Reasons)
	return nil, res
}

func (ev *evaluation) exclude(code string) { ev.exclusions[code]++ }

func (ev *evaluation) observe(f Finding, exclusionsChecked ...string) {
	f.DetectorID, f.DetectorVersion, f.Scope = ev.def.ID, ev.def.Version, ev.ctx.Scope
	f.PlayerID, f.PlayerName = ev.ctx.PlayerID, ev.ctx.PlayerName
	if f.MissingEvidence == nil {
		f.MissingEvidence = []string{}
	}
	if f.EvidenceCompleteness == "" {
		f.EvidenceCompleteness = "COMPLETE"
		if len(f.MissingEvidence) > 0 {
			f.EvidenceCompleteness = "PARTIAL"
		}
	}
	f.ExclusionsChecked = append([]string(nil), exclusionsChecked...)
	sort.Strings(f.ExclusionsChecked)
	ev.candidates = append(ev.candidates, f)
}

// finalize runs the shared VALIDATE stage: deduplicate by incident key,
// apply the owner's sensitivity to the count of independent observations,
// and label tiers. CanNotify additionally requires a released catalog mode.
func (ev *evaluation) finalize() Core8Result {
	res := Core8Result{DetectorID: ev.def.ID, Stage: "VALIDATE", Status: "NO_OBSERVATION",
		Health: AssessCore8Health(ev.def.ID, ev.ctx.Enabled, ev.ctx.Telemetry), Reasons: append([]string{}, ev.reasons...),
		Findings: []Finding{}, Exclusions: ev.exclusions, Enforcement: "DISABLED"}
	seenKey, seenEvidence := map[string]bool{}, map[int64]bool{}
	for _, f := range ev.candidates {
		keyIDs := f.EvidenceIDs
		if len(f.keyEvidenceIDs) > 0 {
			keyIDs = f.keyEvidenceIDs
		}
		key, err := IncidentKey(f.Scope, f.DetectorID, f.DetectorVersion, f.PlayerID, keyIDs)
		if err != nil {
			ev.exclude("INVALID_INCIDENT_EVIDENCE")
			continue
		}
		if seenKey[key] {
			ev.exclude("DUPLICATE_INCIDENT")
			continue
		}
		// An evidence row supports at most one independent observation.
		reused := false
		for _, id := range f.EvidenceIDs {
			reused = reused || seenEvidence[id]
		}
		if reused {
			ev.exclude("EVIDENCE_REUSED")
			continue
		}
		for _, id := range f.EvidenceIDs {
			seenEvidence[id] = true
		}
		seenKey[key] = true
		f.IncidentKey, f.Tier, f.InvestigationStatus = key, TierObserved, "OBSERVATION_ONLY"
		res.Findings = append(res.Findings, f)
	}
	sort.Slice(res.Findings, func(i, j int) bool {
		if !res.Findings[i].EventAt.Equal(res.Findings[j].EventAt) {
			return res.Findings[i].EventAt.Before(res.Findings[j].EventAt)
		}
		return res.Findings[i].IncidentKey < res.Findings[j].IncidentKey
	})
	decision := AssessInvestigation(InvestigationInput{ModuleID: ev.def.ID, Mode: ev.ctx.Mode, Thresholds: ev.ctx.Thresholds,
		SourceCurrent: true, PollingCaughtUp: true, RequiredTelemetryPresent: true,
		ModuleValidated: ev.def.Mode == "VALIDATED_SHADOW", EvidenceProvenanceVerified: true,
		ExclusionsChecked: true, DuplicateFree: true, IndependentObservations: len(res.Findings)})
	res.RequiredObservations = decision.RequiredObservations
	switch {
	case len(res.Findings) == 0:
	case res.RequiredObservations > 0 && len(res.Findings) >= res.RequiredObservations:
		res.Status = "REVIEW_CANDIDATE"
		for i := range res.Findings {
			res.Findings[i].Tier, res.Findings[i].InvestigationStatus = TierSuspicious, "PENDING_STAFF_REVIEW"
		}
	default:
		res.Status = "OBSERVATION_ONLY"
	}
	for _, r := range decision.Reasons {
		if r == "THRESHOLD_NOT_VALIDATED" || r == "MODULE_NOT_VALIDATED" || r == "INVALID_SENSITIVITY" ||
			(r == "INSUFFICIENT_CORROBORATION" && len(res.Findings) > 0) {
			res.Reasons = append(res.Reasons, r)
		}
	}
	if ev.def.Mode != "VALIDATED_SHADOW" {
		res.Reasons = append(res.Reasons, "MODULE_NOT_RELEASED")
	}
	res.CanNotify = res.Status == "REVIEW_CANDIDATE" && ev.def.Mode == "VALIDATED_SHADOW" && res.Health.State == "ACTIVE"
	res.Reasons = uniqueSorted(res.Reasons)
	return res
}

func uniqueSorted(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func coords(x, z float64, alt *float64) Coordinates {
	if alt == nil {
		return Coordinates{X: x, Z: z}
	}
	a := *alt
	return Coordinates{X: x, Z: z, Altitude: &a}
}

func dist2D(ax, az, bx, bz float64) float64 { return math.Hypot(bx-ax, bz-az) }

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// plainDuration renders a duration for staff: "10 seconds", "2 minutes 5 seconds".
func plainDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Second {
		return "under a second"
	}
	unit := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %ss", n, one)
	}
	h, m, sec := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	parts := []string{}
	if h > 0 {
		parts = append(parts, unit(h, "hour"))
	}
	if m > 0 {
		parts = append(parts, unit(m, "minute"))
	}
	if sec > 0 && h == 0 {
		parts = append(parts, unit(sec, "second"))
	}
	return strings.Join(parts, " ")
}
