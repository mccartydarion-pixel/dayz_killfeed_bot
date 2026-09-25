package app

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop/canaryops"
)

// Champion Shop Phase 2C.4 canary operator API (docs/SHOP_DELIVERY_PHASE2C4.md). Organization
// OWNER/ADMIN only, scoped to the installation; mutations additionally need the canary execution lock
// (CHAMPION_SHOP_CANARY_EXECUTION=enabled and the installation in CHAMPION_SHOP_CANARY_INSTALLATION_IDS).
// These routes only RECORD the operator's controlled experiment in the attempt ledger: nothing here
// uploads a file, restarts a server or delivers anything.

const (
	codeCanaryLocked          = "CANARY_EXECUTION_LOCKED"
	codeCanaryNotReady        = "CANARY_NOT_READY"
	codeAttemptNotFound       = "ATTEMPT_NOT_FOUND"
	codeAttemptStateChanged   = "ATTEMPT_STATE_CHANGED"
	codeAttemptConflict       = "ATTEMPT_CONFLICT"
	codeAttemptRefused        = "ATTEMPT_TRANSITION_REFUSED"
	codeEvidenceRequired      = "EVIDENCE_REQUIRED"
	codeEvidenceInvalid       = "EVIDENCE_INVALID"
	codeEvidenceNotAccepted   = "EVIDENCE_NOT_ACCEPTED"
	codeEvidenceAlreadyExists = "EVIDENCE_ALREADY_RECORDED"
	codeAmbiguousBoot         = "AMBIGUOUS_BOOT"
)

func init() {
	httpStatusForCode[codeCanaryLocked] = http.StatusLocked
	httpStatusForCode[codeCanaryNotReady] = http.StatusConflict
	httpStatusForCode[codeAttemptNotFound] = http.StatusNotFound
	httpStatusForCode[codeAttemptStateChanged] = http.StatusConflict
	httpStatusForCode[codeAttemptConflict] = http.StatusConflict
	httpStatusForCode[codeAttemptRefused] = http.StatusConflict
	httpStatusForCode[codeEvidenceRequired] = http.StatusConflict
	httpStatusForCode[codeEvidenceInvalid] = http.StatusUnprocessableEntity
	httpStatusForCode[codeEvidenceNotAccepted] = http.StatusConflict
	httpStatusForCode[codeEvidenceAlreadyExists] = http.StatusConflict
	httpStatusForCode[codeAmbiguousBoot] = http.StatusConflict
}

func (a *App) registerShopCanaryRoutes(base string) {
	h := a.HTTPServer.Handle
	c := base + "/canary/attempts"
	h("GET "+c, a.handleCanaryList)
	h("POST "+c, a.handleCanaryCreate)
	h("GET "+c+"/{attemptID}", a.handleCanaryGet)
	h("POST "+c+"/{attemptID}/advance", a.handleCanaryAdvance)
	h("POST "+c+"/{attemptID}/evidence", a.handleCanaryEvidence)
	h("POST "+c+"/{attemptID}/review", a.handleCanaryReview)
	h("POST "+c+"/{attemptID}/fulfill", a.handleCanaryFulfill)
}

func canaryFailed(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, canaryops.ErrForbidden):
		writeSaaSError(w, codeShopForbidden, "organization OWNER or ADMIN role required")
	case errors.Is(err, canaryops.ErrInstallationUnknown):
		writeSaaSError(w, codeNotFound, "installation not found")
	case errors.Is(err, canaryops.ErrExecutionLocked):
		writeSaaSError(w, codeCanaryLocked, "canary execution is locked for this installation")
	case errors.Is(err, canaryops.ErrSuspended):
		writeSaaSError(w, codeCanaryLocked, "the installation is suspended")
	case errors.Is(err, canaryops.ErrMissingEvidence):
		writeSaaSError(w, codeEvidenceRequired, err.Error())
	case errors.Is(err, canaryops.ErrAmbiguousBoot):
		writeSaaSError(w, codeAmbiguousBoot, err.Error())
	case errors.Is(err, canaryops.ErrInvalid), errors.Is(err, canaryops.ErrNotPhysicalProof), errors.Is(err, canaryops.ErrUseFulfill):
		writeSaaSError(w, codeInvalidRequest, err.Error())
	case errors.Is(err, repository.ErrShopAttemptNotFound):
		writeSaaSError(w, codeAttemptNotFound, "delivery attempt not found")
	case errors.Is(err, repository.ErrShopAttemptStale):
		writeSaaSError(w, codeAttemptStateChanged, "the attempt is no longer in the expected state: reload it")
	case errors.Is(err, repository.ErrShopAttemptConflict), errors.Is(err, repository.ErrShopAttemptDeliveryClosed), errors.Is(err, repository.ErrShopAttemptSequence):
		writeSaaSError(w, codeAttemptConflict, err.Error())
	case errors.Is(err, repository.ErrShopAttemptRejected):
		writeSaaSError(w, codeAttemptRefused, err.Error())
	case errors.Is(err, repository.ErrShopAttemptEvidence):
		writeSaaSError(w, codeEvidenceInvalid, err.Error())
	case errors.Is(err, repository.ErrShopAttemptEvidenceState):
		writeSaaSError(w, codeEvidenceNotAccepted, err.Error())
	case errors.Is(err, repository.ErrShopAttemptEvidenceExists):
		writeSaaSError(w, codeEvidenceAlreadyExists, err.Error())
	case errors.Is(err, repository.ErrShopCanaryBinding), errors.Is(err, repository.ErrShopCanaryNoSession):
		writeSaaSError(w, codeCanaryNotReady, err.Error())
	default:
		shopFailed(w, "canary operation", err)
	}
}

// canaryContext is the Shop admin chain (service auth, acting user, OWNER/ADMIN, installation scope).
func (a *App) canaryContext(w http.ResponseWriter, r *http.Request) (economyRequest, canaryops.Actor, bool) {
	if a.ShopCanary == nil {
		writeSaaSError(w, codeInternalError, "canary operations unavailable")
		return economyRequest{}, canaryops.Actor{}, false
	}
	er, ok := a.shopContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasShopAdminLimiter, rateLimitKey(r)) {
		return economyRequest{}, canaryops.Actor{}, false
	}
	return er, canaryops.Actor{UserID: er.user.ID, DiscordID: er.user.DiscordUserID}, true
}

// --- DTOs -------------------------------------------------------------------------------------------

type canaryAttemptDTO struct {
	AttemptID           string     `json:"attemptId"`
	DeliveryID          int64      `json:"deliveryId"`
	Attempt             int        `json:"attempt"`
	State               string     `json:"state"`
	Fingerprint         string     `json:"fingerprint"`
	ClassName           string     `json:"className"`
	Quantity            int        `json:"quantity"`
	Position            [3]float64 `json:"position"`
	DropSourceFile      string     `json:"dropSourceFile"`
	DropSourceOffset    int64      `json:"dropSourceOffset"`
	StagedSHA256        *string    `json:"stagedSha256"`
	UnstagedSHA256      *string    `json:"unstagedSha256"`
	StagedBootFile      *string    `json:"stagedBootFile"`
	RestartBootFile     *string    `json:"restartBootFile"`
	UnstageVerifiedAt   *time.Time `json:"unstageVerifiedAt"`
	SecondBootFile      *string    `json:"secondBootFile"`
	VerifiedBy          *string    `json:"verifiedBy"`
	FulfilledAt         *time.Time `json:"fulfilledAt"`
	FailureReason       *string    `json:"failureReason"`
	ReviewResolution    *string    `json:"reviewResolution"`
	ReviewResolvedBy    *string    `json:"reviewResolvedBy"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`
	RefundBlocked       bool       `json:"refundBlocked"`
	ManualFulfilBlocked bool       `json:"manualFulfilBlocked"`
}

type canaryEvidenceDTO struct {
	Kind           string     `json:"kind"`
	Source         string     `json:"source"`
	RecordedBy     string     `json:"recordedBy"`
	SHA256         *string    `json:"sha256"`
	PreviousSHA256 *string    `json:"previousSha256"`
	BootFile       *string    `json:"bootFile"`
	BootStartedAt  *time.Time `json:"bootStartedAt"`
	ObservedBy     *string    `json:"observedBy"`
	ObservedAt     time.Time  `json:"observedAt"`
	Detail         string     `json:"detail"`
	RecordedAt     time.Time  `json:"recordedAt"`
	PhysicalProof  bool       `json:"physicalProof"`
}

type canaryEventDTO struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	Actor     string    `json:"actor"`
	Evidence  string    `json:"evidence"`
	CreatedAt time.Time `json:"createdAt"`
}

type canaryViewDTO struct {
	Attempt  canaryAttemptDTO     `json:"attempt"`
	Evidence []canaryEvidenceDTO  `json:"evidence"`
	History  []canaryEventDTO     `json:"history"`
	Next     []canaryops.NextStep `json:"next"`
	Locked   bool                 `json:"executionLocked"`
}

var exposureStates = map[string]bool{repository.AttemptFilePrepared: true, repository.AttemptFileStaged: true, repository.AttemptAwaitingRestart: true,
	repository.AttemptRestartObserved: true, repository.AttemptUnstageRequired: true, repository.AttemptVerificationRequired: true}

func attemptDTO(at repository.ShopAttempt) canaryAttemptDTO {
	unresolved := at.State == repository.AttemptFailedReview && at.ReviewResolution == nil
	refundBlocked := exposureStates[at.State] || (at.State == repository.AttemptFailedReview && (at.ReviewResolution == nil || *at.ReviewResolution != repository.ReviewNotSpawned))
	return canaryAttemptDTO{AttemptID: at.AttemptID, DeliveryID: at.DeliveryID, Attempt: at.Attempt, State: at.State, Fingerprint: at.Fingerprint,
		ClassName: at.ClassName, Quantity: at.Quantity, Position: [3]float64{at.PosX, at.PosY, at.PosZ}, DropSourceFile: at.DropSourceFile,
		DropSourceOffset: at.DropSourceOffset, StagedSHA256: at.StagedSHA256, UnstagedSHA256: at.UnstagedSHA256, StagedBootFile: at.StagedBootFile,
		RestartBootFile: at.RestartBootFile, UnstageVerifiedAt: at.UnstageVerifiedAt, SecondBootFile: at.SecondBootFile, VerifiedBy: at.VerifiedBy,
		FulfilledAt: at.FulfilledAt, FailureReason: at.FailureReason, ReviewResolution: at.ReviewResolution, ReviewResolvedBy: at.ReviewResolvedBy,
		CreatedAt: at.CreatedAt, UpdatedAt: at.UpdatedAt, RefundBlocked: refundBlocked, ManualFulfilBlocked: exposureStates[at.State] || unresolved}
}

func (a *App) viewDTO(inst int64, v canaryops.AttemptView) canaryViewDTO {
	out := canaryViewDTO{Attempt: attemptDTO(v.Attempt), Evidence: []canaryEvidenceDTO{}, History: []canaryEventDTO{}, Next: v.Next,
		Locked: !a.ShopCanaryGate.Allows(inst)}
	for _, e := range v.Evidence {
		out.Evidence = append(out.Evidence, canaryEvidenceDTO{Kind: e.Kind, Source: e.Source, RecordedBy: e.RecordedBy, SHA256: e.SHA256,
			PreviousSHA256: e.PreviousSHA256, BootFile: e.BootFile, BootStartedAt: e.BootStartedAt, ObservedBy: e.ObservedBy, ObservedAt: e.ObservedAt,
			Detail: e.Detail, RecordedAt: e.RecordedAt, PhysicalProof: e.Source == repository.SourceInGameObservation})
	}
	for _, e := range v.Events {
		out.History = append(out.History, canaryEventDTO{From: e.FromState, To: e.ToState, Actor: e.Actor, Evidence: e.Evidence, CreatedAt: e.CreatedAt})
	}
	return out
}

// --- handlers ---------------------------------------------------------------------------------------

func canaryCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), economyTimeout)
}

func (a *App) handleCanaryList(w http.ResponseWriter, r *http.Request) {
	er, actor, ok := a.canaryContext(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ctx, cancel := canaryCtx(r)
	defer cancel()
	list, err := a.ShopCanary.ListAttempts(ctx, er.scope.OrganizationID, er.scope.InstallationID, actor, r.URL.Query().Get("open") == "true", limit)
	if err != nil {
		canaryFailed(w, err)
		return
	}
	out := []canaryAttemptDTO{}
	for _, at := range list {
		out = append(out, attemptDTO(at))
	}
	writeSaaSJSON(w, http.StatusOK, struct {
		Attempts []canaryAttemptDTO `json:"attempts"`
		Locked   bool               `json:"executionLocked"`
	}{out, !a.ShopCanaryGate.Allows(er.scope.InstallationID)})
}

func (a *App) handleCanaryGet(w http.ResponseWriter, r *http.Request) {
	er, actor, ok := a.canaryContext(w, r)
	if !ok {
		return
	}
	ctx, cancel := canaryCtx(r)
	defer cancel()
	v, err := a.ShopCanary.GetAttempt(ctx, er.scope.OrganizationID, er.scope.InstallationID, actor, r.PathValue("attemptID"))
	if err != nil {
		canaryFailed(w, err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, a.viewDTO(er.scope.InstallationID, v))
}

type canaryCreateBody struct {
	DeliveryID       int64     `json:"deliveryId"`
	AltitudeY        *float64  `json:"altitudeY"`
	DropSourceFile   string    `json:"dropSourceFile"`
	DropSourceOffset int64     `json:"dropSourceOffset"`
	DropObservedAt   time.Time `json:"dropObservedAt"`
}

func (a *App) handleCanaryCreate(w http.ResponseWriter, r *http.Request) {
	er, actor, ok := a.canaryContext(w, r)
	if !ok {
		return
	}
	var body canaryCreateBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	if body.AltitudeY == nil {
		writeSaaSError(w, codeInvalidRequest, "altitudeY (the drop point's ADM altitude) is required")
		return
	}
	ctx, cancel := canaryCtx(r)
	defer cancel()
	v, err := a.ShopCanary.CreateAttempt(ctx, er.scope.OrganizationID, er.scope.InstallationID, actor, canaryops.CreateRequest{
		DeliveryID: body.DeliveryID, AltitudeY: *body.AltitudeY, DropSourceFile: body.DropSourceFile, DropSourceOffset: body.DropSourceOffset, DropObservedAt: body.DropObservedAt})
	if err != nil {
		canaryFailed(w, err)
		return
	}
	shopAudit("shop_canary_attempt_created", er, "attempt_id", v.Attempt.AttemptID, "delivery_id", v.Attempt.DeliveryID)
	writeSaaSJSON(w, http.StatusCreated, a.viewDTO(er.scope.InstallationID, v))
}

type canaryAdvanceBody struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

func (a *App) handleCanaryAdvance(w http.ResponseWriter, r *http.Request) {
	er, actor, ok := a.canaryContext(w, r)
	if !ok {
		return
	}
	var body canaryAdvanceBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	ctx, cancel := canaryCtx(r)
	defer cancel()
	v, err := a.ShopCanary.AdvanceAttempt(ctx, er.scope.OrganizationID, er.scope.InstallationID, actor, r.PathValue("attemptID"), body.From, body.To, body.Reason)
	if err != nil {
		canaryFailed(w, err)
		return
	}
	shopAudit("shop_canary_attempt_advanced", er, "attempt_id", v.Attempt.AttemptID, "from", body.From, "to", body.To)
	writeSaaSJSON(w, http.StatusOK, a.viewDTO(er.scope.InstallationID, v))
}

type canaryEvidenceBody struct {
	Kind           string     `json:"kind"`
	Source         string     `json:"source"`
	SHA256         string     `json:"sha256"`
	PreviousSHA256 string     `json:"previousSha256"`
	BootFile       string     `json:"bootFile"`
	BootStartedAt  *time.Time `json:"bootStartedAt"`
	ObservedBy     string     `json:"observedBy"`
	ObservedAt     time.Time  `json:"observedAt"`
	Detail         string     `json:"detail"`
}

func (a *App) handleCanaryEvidence(w http.ResponseWriter, r *http.Request) {
	er, actor, ok := a.canaryContext(w, r)
	if !ok {
		return
	}
	var b canaryEvidenceBody
	if !decodeFactionBody(w, r, &b) {
		return
	}
	ctx, cancel := canaryCtx(r)
	defer cancel()
	v, err := a.ShopCanary.RecordEvidence(ctx, er.scope.OrganizationID, er.scope.InstallationID, actor, r.PathValue("attemptID"), canaryops.EvidenceRequest{
		Kind: b.Kind, Source: b.Source, SHA256: b.SHA256, PreviousSHA256: b.PreviousSHA256, BootFile: b.BootFile, BootStartedAt: b.BootStartedAt,
		ObservedBy: b.ObservedBy, ObservedAt: b.ObservedAt, Detail: b.Detail})
	if err != nil {
		canaryFailed(w, err)
		return
	}
	shopAudit("shop_canary_evidence_recorded", er, "attempt_id", v.Attempt.AttemptID, "kind", b.Kind, "source", b.Source)
	writeSaaSJSON(w, http.StatusCreated, a.viewDTO(er.scope.InstallationID, v))
}

type canaryReviewBody struct {
	Outcome string `json:"outcome"`
	Note    string `json:"note"`
}

func (a *App) handleCanaryReview(w http.ResponseWriter, r *http.Request) {
	er, actor, ok := a.canaryContext(w, r)
	if !ok {
		return
	}
	var b canaryReviewBody
	if !decodeFactionBody(w, r, &b) {
		return
	}
	ctx, cancel := canaryCtx(r)
	defer cancel()
	v, err := a.ShopCanary.ResolveReview(ctx, er.scope.OrganizationID, er.scope.InstallationID, actor, r.PathValue("attemptID"), b.Outcome, b.Note)
	if err != nil {
		canaryFailed(w, err)
		return
	}
	shopAudit("shop_canary_review_recorded", er, "attempt_id", v.Attempt.AttemptID, "outcome", b.Outcome)
	writeSaaSJSON(w, http.StatusOK, a.viewDTO(er.scope.InstallationID, v))
}

type canaryFulfillBody struct {
	Note string `json:"note"`
}

func (a *App) handleCanaryFulfill(w http.ResponseWriter, r *http.Request) {
	er, actor, ok := a.canaryContext(w, r)
	if !ok {
		return
	}
	var b canaryFulfillBody
	if !decodeFactionBody(w, r, &b) {
		return
	}
	ctx, cancel := canaryCtx(r)
	defer cancel()
	v, err := a.ShopCanary.FulfillAttempt(ctx, er.scope.OrganizationID, er.scope.InstallationID, actor, r.PathValue("attemptID"), b.Note)
	if err != nil {
		canaryFailed(w, err)
		return
	}
	shopAudit("shop_canary_attempt_fulfilled", er, "attempt_id", v.Attempt.AttemptID, "delivery_id", v.Attempt.DeliveryID)
	writeSaaSJSON(w, http.StatusOK, a.viewDTO(er.scope.InstallationID, v))
}
