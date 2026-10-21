// Package casealert holds a pure, offline-only eligibility contract for a
// future private reviewed-finding notification. No route, sender, DB or worker
// calls this package in production; a plan is never permission to send.
package casealert

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

type Scope struct{ GuildID, InstallationID, ServerID int64 }

type Request struct {
	Scope               Scope
	EvidenceScope       Scope
	RouteScope          Scope
	FindingID           string // opaque, never a player identity
	EventVersion        int64  // immutable revision for delivery deduplication
	DetectorID          string
	EvidenceRef         string // opaque authorized reference, not a raw source path
	ReviewState         string // explicit human review state
	SourceMatched       bool
	QualityAccepted     bool
	DetectorValidated   bool
	ReviewerAuthorized  bool
	EvidenceAccessible  bool
	StaffAlertsEnabled  bool
	RoutePrivate        bool
	RouteReachable      bool
	EnforcementDisabled bool
}

type Plan struct {
	Eligible bool
	Key      string // deterministic opaque idempotency key; only populated when eligible
	Blockers []string
}

// Evaluate computes a fail-closed candidate plan from synthetic, independently
// supplied flags. A future real publisher MUST independently re-authorize all
// claims inside the durable transaction and again just before delivery.
func Evaluate(in Request) Plan {
	out := Plan{Blockers: make([]string, 0, 12)}
	add := func(code string) { out.Blockers = append(out.Blockers, code) }
	validScope := func(s Scope) bool { return s.GuildID > 0 && s.InstallationID > 0 && s.ServerID > 0 }
	if !validScope(in.Scope) {
		add("INVALID_INSTALLATION_SCOPE")
	}
	if !validScope(in.EvidenceScope) || in.EvidenceScope != in.Scope {
		add("EVIDENCE_SCOPE_MISMATCH")
	}
	if !validScope(in.RouteScope) || in.RouteScope != in.Scope {
		add("PRIVATE_ROUTE_SCOPE_MISMATCH")
	}
	if strings.TrimSpace(in.FindingID) == "" || strings.TrimSpace(in.DetectorID) == "" ||
		strings.TrimSpace(in.EvidenceRef) == "" || in.EventVersion <= 0 {
		add("MISSING_IMMUTABLE_FINDING_REFERENCE")
	}
	if strings.ContainsAny(in.EvidenceRef, "/\\") {
		add("INVALID_EVIDENCE_REFERENCE")
	}
	if in.ReviewState != "REVIEWED" {
		add("STAFF_REVIEW_REQUIRED")
	}
	if !in.SourceMatched {
		add("CURRENT_SOURCE_NOT_VERIFIED")
	}
	if !in.QualityAccepted {
		add("EVIDENCE_QUALITY_NOT_ACCEPTED")
	}
	if !in.DetectorValidated {
		add("DETECTOR_NOT_VALIDATED")
	}
	if !in.ReviewerAuthorized {
		add("REVIEWER_NOT_AUTHORIZED")
	}
	if !in.EvidenceAccessible {
		add("EVIDENCE_LINK_NOT_AUTHORIZED")
	}
	if !in.StaffAlertsEnabled {
		add("STAFF_DELIVERY_NOT_ENABLED")
	}
	if !in.RoutePrivate || !in.RouteReachable {
		add("PRIVATE_ROUTE_UNAVAILABLE")
	}
	if !in.EnforcementDisabled {
		add("ENFORCEMENT_MUST_REMAIN_DISABLED")
	}
	if len(out.Blockers) > 0 {
		return out
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("case-private-notification-v1:%d:%d:%d:%s:%d",
		in.Scope.GuildID, in.Scope.InstallationID, in.Scope.ServerID, in.FindingID, in.EventVersion)))
	out.Key = hex.EncodeToString(sum[:])
	out.Eligible = true
	return out
}
