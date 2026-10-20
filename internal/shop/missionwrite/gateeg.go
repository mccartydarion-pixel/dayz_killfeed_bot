package missionwrite

import (
	"bytes"
	"errors"

	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop/canary"
	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

// Gate E (stage) and Gate G (unstage) - docs/SHOP_CANARY_STAGING.md. Both write only the Champion
// file custom/champion_shop_delivery.json, and only for ONE ledger attempt whose immutable plan facts
// (attempt id, class, quantity, position, artifact path) are read from the attempt ledger by the caller.
// The payload is derived from those facts (nitradodelivery.SingleAttemptFiles), never supplied:
//
//	Gate E: attempt FILE_PREPARED; the file is exactly the empty file -> this attempt's staged file.
//	Gate G: attempt FILE_STAGED, AWAITING_RESTART (abort before any start) or UNSTAGE_REQUIRED;
//	        the file is exactly this attempt's staged file -> the empty file.
//
// Neither writes the database: the command prints the verified read-back hashes the operator records
// through the canary operator API, which re-checks them against the same facts (ARTIFACT_HASH_MISMATCH).

// AttemptFacts are one attempt's immutable plan facts and current state, as the ledger holds them.
type AttemptFacts struct {
	AttemptID    string
	State        string
	ClassName    string
	Quantity     int
	Pos          [3]float64
	ArtifactPath string
}

var (
	ErrAttemptRequired   = errors.New("missionwrite: this operation needs the ledger attempt it stages or unstages")
	ErrAttemptState      = errors.New("missionwrite: the attempt is not in a state that allows this write")
	ErrAttemptArtifact   = errors.New("missionwrite: the attempt does not stage the custom/ Champion file")
	ErrNotReferenced     = errors.New("missionwrite: cfggameplay.json does not reference the Champion file")
	stageFrom            = map[string]bool{repository.AttemptFilePrepared: true}
	unstageFrom          = map[string]bool{repository.AttemptFileStaged: true, repository.AttemptAwaitingRestart: true, repository.AttemptUnstageRequired: true}
	errEmptyFileMismatch = errors.New("missionwrite: internal: the attempt's empty file differs from the Gate C empty file")
)

// AttemptFiles returns the staged and empty Champion file for the attempt.
func AttemptFiles(a AttemptFacts) (staged, empty []byte) {
	return nitradodelivery.SingleAttemptFiles(a.AttemptID, a.ClassName, a.Quantity, a.Pos)
}

// attemptPayload validates the attempt for the operation and returns (payload, expected current SHA-256).
func attemptPayload(kind opKind, a *AttemptFacts) ([]byte, string, error) {
	if a == nil || a.AttemptID == "" {
		return nil, "", ErrAttemptRequired
	}
	if a.ArtifactPath != nitradodelivery.ArtifactRelPath {
		return nil, "", ErrAttemptArtifact
	}
	if err := canary.ValidateProductionAttemptID(a.AttemptID); err != nil {
		return nil, "", err
	}
	staged, empty := AttemptFiles(*a)
	if gate, _ := canary.EmptyArtifact(); !bytes.Equal(empty, gate) {
		return nil, "", errEmptyFileMismatch
	}
	switch kind {
	case kindStage:
		if !stageFrom[a.State] {
			return nil, "", ErrAttemptState
		}
		return staged, SHA256(empty), nil
	case kindUnstage:
		if !unstageFrom[a.State] {
			return nil, "", ErrAttemptState
		}
		return empty, SHA256(staged), nil
	}
	return nil, "", ErrUnknownOperation
}

// withAttemptPayload fills the derived payload for Gate E / Gate G (Validate has already checked it).
func withAttemptPayload(r Request) Request {
	if spec := ops[r.Operation]; spec != nil && isAttemptKind(spec.kind) {
		if p, _, err := attemptPayload(spec.kind, r.Attempt); err == nil {
			r.Payload = p
		}
	}
	return r
}

// attemptLine binds a plan to the attempt and its state (empty for operations without an attempt, so
// earlier plan IDs are unchanged).
func attemptLine(a *AttemptFacts) string {
	if a == nil {
		return ""
	}
	return "attempt=" + a.AttemptID + " state=" + a.State + " artifact=" + a.ArtifactPath
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}
