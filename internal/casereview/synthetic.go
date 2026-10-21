// Package casereview contains an offline, synthetic case-review state machine.
//
// Nothing in this package admits live findings, authenticates a user, stores a
// record, publishes a Discord alert or performs enforcement. Live admission,
// authentication, durable tenant isolation and delivery are separate gates.
package casereview

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type Status string

const (
	Draft         Status = "DRAFT"
	PendingReview Status = "PENDING_REVIEW"
	Dismissed     Status = "DISMISSED"
	Reviewed      Status = "REVIEWED"
	Resolved      Status = "RESOLVED"
)

type Scope struct {
	GuildID        int64
	InstallationID int64
	ServerID       int64
}

type SyntheticInput struct {
	ID               string
	Scope            Scope
	DetectorID       string
	DetectorVersion  string
	EvidenceIDs      []int64
	SourceQualityRef string // opaque fixture reference, never a raw ADM path
	At               time.Time
}

type Reviewer struct {
	ID         string
	Authorized bool // test fixture only; production must validate independently
}

type Action struct {
	ID   string // idempotency key, unique per case
	To   Status
	Note string
	At   time.Time
}

type Audit struct {
	ActionID   string
	From       Status
	To         Status
	ReviewerID string
	Note       string
	At         time.Time
}

type Case struct {
	id               string
	scope            Scope
	detectorID       string
	detectorVersion  string
	evidenceIDs      []int64
	sourceQualityRef string
	openedAt         time.Time
	status           Status
	history          []Audit
}

// NewSynthetic creates a fixture-only draft, not a real finding. Only the
// separate, reviewed production admission gate may create live case records.
func NewSynthetic(in SyntheticInput) (*Case, error) {
	if strings.TrimSpace(in.ID) == "" || in.Scope.GuildID <= 0 || in.Scope.InstallationID <= 0 || in.Scope.ServerID <= 0 ||
		strings.TrimSpace(in.DetectorID) == "" || strings.TrimSpace(in.DetectorVersion) == "" ||
		strings.TrimSpace(in.SourceQualityRef) == "" || in.At.IsZero() || len(in.EvidenceIDs) == 0 || len(in.EvidenceIDs) > 50 {
		return nil, errors.New("invalid synthetic case identity or provenance")
	}
	if strings.ContainsAny(in.SourceQualityRef, "/\\") {
		return nil, errors.New("source quality reference must be opaque")
	}
	seen := make(map[int64]bool, len(in.EvidenceIDs))
	for _, id := range in.EvidenceIDs {
		if id <= 0 || seen[id] {
			return nil, errors.New("invalid or repeated evidence ID")
		}
		seen[id] = true
	}
	return &Case{id: in.ID, scope: in.Scope, detectorID: in.DetectorID, detectorVersion: in.DetectorVersion,
		evidenceIDs: append([]int64(nil), in.EvidenceIDs...), sourceQualityRef: in.SourceQualityRef,
		openedAt: in.At.UTC(), status: Draft, history: make([]Audit, 0)}, nil
}

type Snapshot struct {
	ID               string
	Scope            Scope
	DetectorID       string
	DetectorVersion  string
	EvidenceIDs      []int64
	SourceQualityRef string
	Synthetic        bool
	OpenedAt         time.Time
	Status           Status
	History          []Audit
}

func (c *Case) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	return Snapshot{ID: c.id, Scope: c.scope, DetectorID: c.detectorID, DetectorVersion: c.detectorVersion,
		EvidenceIDs: append([]int64(nil), c.evidenceIDs...), SourceQualityRef: c.sourceQualityRef,
		Synthetic: true, OpenedAt: c.openedAt, Status: c.status, History: append([]Audit(nil), c.history...)}
}

func allowed(from, to Status) bool {
	switch from {
	case Draft:
		return to == PendingReview || to == Dismissed
	case PendingReview:
		return to == Reviewed || to == Dismissed
	case Reviewed:
		return to == Resolved
	default:
		return false
	}
}

// ApplySynthetic applies an explicitly authorized test action. It is not a
// replacement for live server-side staff authentication or persisted auditing.
// Duplicate IDs with identical actions are no-ops; collisions fail closed.
func (c *Case) ApplySynthetic(reviewer Reviewer, a Action) (bool, error) {
	if c == nil {
		return false, errors.New("synthetic case unavailable")
	}
	if !reviewer.Authorized || strings.TrimSpace(reviewer.ID) == "" {
		return false, errors.New("reviewer authorization required")
	}
	if strings.TrimSpace(a.ID) == "" || a.At.IsZero() || strings.TrimSpace(a.Note) == "" ||
		len([]rune(a.Note)) > 500 || strings.ContainsAny(a.Note, "@") {
		return false, errors.New("invalid action ID, timestamp or neutral note")
	}
	at := a.At.UTC()
	for _, previous := range c.history {
		if previous.ActionID != a.ID {
			continue
		}
		if previous.To == a.To && previous.ReviewerID == reviewer.ID &&
			previous.Note == a.Note && previous.At.Equal(at) {
			return false, nil
		}
		return false, errors.New("action ID conflicts with recorded review")
	}
	if !allowed(c.status, a.To) {
		return false, fmt.Errorf("invalid synthetic transition: %s to %s", c.status, a.To)
	}
	last := c.openedAt
	if n := len(c.history); n > 0 {
		last = c.history[n-1].At
	}
	if at.Before(last) {
		return false, errors.New("review time precedes prior record")
	}
	c.history = append(c.history, Audit{ActionID: a.ID, From: c.status, To: a.To,
		ReviewerID: reviewer.ID, Note: a.Note, At: at})
	c.status = a.To
	return true, nil
}
