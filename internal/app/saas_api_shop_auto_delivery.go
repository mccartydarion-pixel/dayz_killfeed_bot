package app

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

// Shop automatic delivery: the routes around the worker (docs/SHOP_DELIVERY_WORKER_DESIGN.md).
// None of them writes to a game server; they are the switches, the buyer's drop position and the
// staff review of an order the worker stopped on.
//
//	Staff (organization OWNER/ADMIN):
//	  GET  .../shop/admin/auto-delivery                          the installation's switch, pause and worker mode
//	  PUT  .../shop/admin/auto-delivery                          {"enabled": true|false}
//	  POST .../shop/admin/auto-delivery/resume                   clear a pause (a person's decision)
//	  GET  .../shop/admin/products/{productID}/auto-delivery     the product's switch and class name
//	  PUT  .../shop/admin/products/{productID}/auto-delivery     {"autoDelivery": true|false, "className": "..."}
//	  GET  .../shop/admin/purchases/{purchaseID}/delivery-attempts
//	  POST .../shop/admin/delivery-attempts/{attemptID}/review   {"outcome": "NOT_SPAWNED|SPAWNED", "observer": "...", "detail": "..."}
//	Buyer:
//	  GET  .../shop/me/delivery-position?productId=N             where an automatic order would be dropped
const (
	codeDeliveryPositionRequired = "DELIVERY_POSITION_REQUIRED"
	codeAutoDeliveryNotAllowed   = "AUTO_DELIVERY_NOT_ALLOWED"
	codeAttemptNotReviewable     = "ATTEMPT_NOT_REVIEWABLE"
)

func init() {
	httpStatusForCode[codeDeliveryPositionRequired] = http.StatusConflict
	httpStatusForCode[codeAutoDeliveryNotAllowed] = http.StatusConflict
	httpStatusForCode[codeAttemptNotReviewable] = http.StatusConflict
}

// shopDropPositionMaxAge is how old the buyer's logged position may be when they buy. The server
// logs every online player every five minutes.
const shopDropPositionMaxAge = 20 * time.Minute

// shopDropPositionTolerance is how far the coordinates of an order may be from that position.
const shopDropPositionTolerance = 0.05

func (a *App) registerShopAutoDeliveryRoutes(base string) {
	h := a.HTTPServer.Handle
	h("GET "+base+"/admin/auto-delivery", a.handleShopAutoDeliveryGet)
	h("PUT "+base+"/admin/auto-delivery", a.handleShopAutoDeliverySet)
	h("POST "+base+"/admin/auto-delivery/resume", a.handleShopAutoDeliveryResume)
	h("GET "+base+"/admin/products/{productID}/auto-delivery", a.handleShopProductAutoDeliveryGet)
	h("PUT "+base+"/admin/products/{productID}/auto-delivery", a.handleShopProductAutoDeliverySet)
	h("GET "+base+"/admin/purchases/{purchaseID}/delivery-attempts", a.handleShopPurchaseAttempts)
	h("POST "+base+"/admin/delivery-attempts/{attemptID}/review", a.handleShopAttemptReview)
	h("GET "+base+"/me/delivery-position", a.handleShopDeliveryPosition)
}

// shopAutoContext is shopContext plus the worker's store.
func (a *App) shopAutoContext(w http.ResponseWriter, r *http.Request, admin bool) (economyRequest, bool) {
	if a.ShopAuto == nil || a.shopAttempts == nil {
		writeSaaSError(w, codeInternalError, "shop automatic delivery unavailable")
		return economyRequest{}, false
	}
	return a.shopContext(w, r, admin)
}

// shopWorkerMode is the worker's mode for one installation: "off" unless the server lock lists it.
func (a *App) shopWorkerMode(installationID int64) string {
	if a.Config == nil {
		return "off"
	}
	lock := a.Config.ShopAutoDelivery
	for _, id := range lock.InstallationIDs {
		if id == installationID && lock.Mode != config.ShopAutoDeliveryOff {
			return lock.Mode
		}
	}
	return "off"
}

type shopAutoDeliveryDTO struct {
	Enabled      bool       `json:"enabled"`
	Paused       bool       `json:"paused"`
	PausedReason string     `json:"pausedReason"`
	PausedAt     *time.Time `json:"pausedAt"`
	// Worker is "off", "report" or "enabled": whether a worker runs for this installation at all.
	// It is set on the server, never from the website.
	Worker string `json:"worker"`
	// Delivering is true only when every switch is on: orders of automatic products are delivered.
	Delivering bool `json:"delivering"`
}

func (a *App) autoDeliveryDTO(installationID int64, s repository.ShopAutoDeliverySettings) shopAutoDeliveryDTO {
	mode := a.shopWorkerMode(installationID)
	return shopAutoDeliveryDTO{Enabled: s.Enabled, Paused: s.PausedAt != nil, PausedReason: s.PausedReason, PausedAt: s.PausedAt, Worker: mode,
		Delivering: s.Enabled && s.PausedAt == nil && mode == config.ShopAutoDeliveryEnabled}
}

func (a *App) writeAutoDelivery(w http.ResponseWriter, ctx context.Context, er economyRequest) {
	s, err := a.ShopAuto.Settings(ctx, er.scope.OrganizationID, er.scope.InstallationID)
	if err != nil {
		shopFailed(w, "load automatic delivery settings", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, struct {
		AutoDelivery shopAutoDeliveryDTO `json:"autoDelivery"`
	}{a.autoDeliveryDTO(er.scope.InstallationID, s)})
}

func (a *App) handleShopAutoDeliveryGet(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopAutoContext(w, r, true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	a.writeAutoDelivery(w, ctx, er)
}

func (a *App) handleShopAutoDeliverySet(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopAutoContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasShopAdminLimiter, rateLimitKey(r)) {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeFactionBody(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeSaaSError(w, codeInvalidRequest, "enabled must be true or false")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	if err := a.ShopAuto.SetEnabled(ctx, er.scope.OrganizationID, er.scope.InstallationID, *body.Enabled, er.user.ID); err != nil {
		shopFailed(w, "set automatic delivery", err)
		return
	}
	shopAudit("shop_auto_delivery_switched", er, "enabled", *body.Enabled)
	a.writeAutoDelivery(w, ctx, er)
}

// handleShopAutoDeliveryResume clears a pause. The worker paused because something on the server
// was not what it expected; resuming is a person saying they checked.
func (a *App) handleShopAutoDeliveryResume(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopAutoContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasShopAdminLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	before, err := a.ShopAuto.Settings(ctx, er.scope.OrganizationID, er.scope.InstallationID)
	if err != nil {
		shopFailed(w, "resume automatic delivery", err)
		return
	}
	if err := a.ShopAuto.Resume(ctx, er.scope.OrganizationID, er.scope.InstallationID, er.user.ID); err != nil {
		shopFailed(w, "resume automatic delivery", err)
		return
	}
	shopAudit("shop_auto_delivery_resumed", er, "was_paused", before.PausedAt != nil, "paused_reason", before.PausedReason)
	a.writeAutoDelivery(w, ctx, er)
}

type shopProductAutoDeliveryDTO struct {
	ProductID      int64  `json:"productId"`
	AutoDelivery   bool   `json:"autoDelivery"`
	ClassName      string `json:"className"`
	DeliveryPolicy string `json:"deliveryPolicy"`
}

func productAutoDTO(p repository.ShopProductAutoDelivery) shopProductAutoDeliveryDTO {
	return shopProductAutoDeliveryDTO{ProductID: p.ProductID, AutoDelivery: p.AutoDelivery, ClassName: p.ClassName, DeliveryPolicy: p.DeliveryPolicy}
}

func (a *App) handleShopProductAutoDeliveryGet(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopAutoContext(w, r, true)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "productID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	p, err := a.ShopAuto.ProductAutoDelivery(ctx, er.scope.OrganizationID, er.scope.InstallationID, id)
	if err != nil {
		shopFailed(w, "load product automatic delivery", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, struct {
		Product shopProductAutoDeliveryDTO `json:"product"`
	}{productAutoDTO(p)})
}

func (a *App) handleShopProductAutoDeliverySet(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopAutoContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasShopAdminLimiter, rateLimitKey(r)) {
		return
	}
	id, ok := pathInt64(w, r, "productID")
	if !ok {
		return
	}
	var body struct {
		AutoDelivery *bool  `json:"autoDelivery"`
		ClassName    string `json:"className"`
	}
	if !decodeFactionBody(w, r, &body) {
		return
	}
	class := strings.TrimSpace(body.ClassName)
	switch {
	case body.AutoDelivery == nil:
		writeSaaSError(w, codeInvalidRequest, "autoDelivery must be true or false")
		return
	case class != "" && nitradodelivery.ValidateClassName(class) != nil:
		writeSaaSError(w, codeInvalidRequest, "className must be a DayZ class name: letters, digits and underscores, starting with a letter")
		return
	case *body.AutoDelivery && class == "":
		writeSaaSError(w, codeInvalidRequest, "className is required for automatic delivery")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	p, err := a.ShopAuto.SetProductAutoDelivery(ctx, er.scope.OrganizationID, er.scope.InstallationID, id, *body.AutoDelivery, class)
	if errors.Is(err, repository.ErrShopProductNotCoordinate) {
		writeSaaSError(w, codeAutoDeliveryNotAllowed, "only a product with coordinate delivery can be delivered automatically")
		return
	}
	if err != nil {
		shopFailed(w, "set product automatic delivery", err)
		return
	}
	shopAudit("shop_product_auto_delivery_set", er, "product_id", id, "auto_delivery", p.AutoDelivery, "class_name", p.ClassName)
	writeSaaSJSON(w, http.StatusOK, struct {
		Product shopProductAutoDeliveryDTO `json:"product"`
	}{productAutoDTO(p)})
}

// shopAutomatic reports whether an order of this product would be delivered automatically right now:
// the worker delivers for this installation, the owner's switch is on, nothing is paused, and the
// product is automatic with a class name and coordinate delivery.
func (a *App) shopAutomatic(ctx context.Context, scope repository.EconomyScope, productID int64) (bool, error) {
	if a.ShopAuto == nil || a.shopWorkerMode(scope.InstallationID) != config.ShopAutoDeliveryEnabled {
		return false, nil
	}
	s, err := a.ShopAuto.Settings(ctx, scope.OrganizationID, scope.InstallationID)
	if err != nil || !s.Enabled || s.PausedAt != nil {
		return false, err
	}
	p, err := a.ShopAuto.ProductAutoDelivery(ctx, scope.OrganizationID, scope.InstallationID, productID)
	if errors.Is(err, repository.ErrShopProductNotFound) {
		return false, nil
	}
	return err == nil && p.AutoDelivery && p.ClassName != "" && p.DeliveryPolicy == repository.DeliveryPolicyManualCoordinate, err
}

type shopDropPositionDTO struct {
	X          float64   `json:"x"`
	Z          float64   `json:"z"`
	ObservedAt time.Time `json:"observedAt"`
	AgeSeconds int64     `json:"ageSeconds"`
	// Fresh is false when the position is too old to buy with: the buyer waits for the next update.
	Fresh bool `json:"fresh"`
}

// buyerPosition is the acting buyer's latest logged position in the current boot, if any.
func (a *App) buyerPosition(ctx context.Context, er economyRequest) (*shopDropPositionDTO, error) {
	if er.scope.ServerID == 0 {
		return nil, nil
	}
	account, err := a.EconomyAccounts.Me(ctx, er.scope, er.user.DiscordUserID)
	if err != nil {
		return nil, err
	}
	p, ok, err := a.ShopAuto.LatestPosition(ctx, er.scope.ServerID, account.AccountID)
	if err != nil || !ok {
		return nil, err
	}
	age := time.Since(p.ObservedAt)
	return &shopDropPositionDTO{X: p.X, Z: p.Z, ObservedAt: p.ObservedAt, AgeSeconds: int64(age / time.Second), Fresh: age >= 0 && age <= shopDropPositionMaxAge}, nil
}

// handleShopDeliveryPosition tells the buyer whether this product is delivered automatically and,
// if so, where: their own last position the server logged. The altitude is never shown or accepted
// from the client; the worker takes it from the same log line.
func (a *App) handleShopDeliveryPosition(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopAutoContext(w, r, false)
	if !ok {
		return
	}
	productID, err := strconv.ParseInt(r.URL.Query().Get("productId"), 10, 64)
	if err != nil || productID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "productId must be a positive whole number")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	automatic, err := a.shopAutomatic(ctx, er.scope, productID)
	if err != nil {
		shopFailed(w, "load delivery position", err)
		return
	}
	out := struct {
		Automatic     bool                 `json:"automatic"`
		Position      *shopDropPositionDTO `json:"position"`
		MaxAgeSeconds int64                `json:"maxAgeSeconds"`
	}{Automatic: automatic, MaxAgeSeconds: int64(shopDropPositionMaxAge / time.Second)}
	if automatic {
		if out.Position, err = a.buyerPosition(ctx, er); err != nil {
			shopFailed(w, "load delivery position", err)
			return
		}
	}
	writeSaaSJSON(w, http.StatusOK, out)
}

// requireDropPosition is the purchase gate for automatic products: the order's coordinates must be
// the buyer's own fresh logged position. It writes the error and returns false to stop the purchase.
// For any other product it does nothing.
func (a *App) requireDropPosition(w http.ResponseWriter, ctx context.Context, er economyRequest, productID int64, x, z *float64) bool {
	automatic, err := a.shopAutomatic(ctx, er.scope, productID)
	if err != nil {
		shopFailed(w, "check delivery position", err)
		return false
	}
	if !automatic {
		return true
	}
	pos, err := a.buyerPosition(ctx, er)
	if err != nil {
		shopFailed(w, "check delivery position", err)
		return false
	}
	switch {
	case pos == nil:
		writeSaaSError(w, codeDeliveryPositionRequired, "this item is delivered to where you are standing: join the server and wait a few minutes for your position to register")
	case !pos.Fresh:
		writeSaaSError(w, codeDeliveryPositionRequired, "your last known position is too old: stay in game and try again in a few minutes")
	case x == nil || z == nil || math.Abs(*x-pos.X) > shopDropPositionTolerance || math.Abs(*z-pos.Z) > shopDropPositionTolerance:
		writeSaaSError(w, codeDeliveryPositionRequired, "your position changed: check the delivery position shown and confirm again")
	default:
		return true
	}
	return false
}

type shopAttemptSummaryDTO struct {
	AttemptID        string     `json:"attemptId"`
	State            string     `json:"state"`
	Automatic        bool       `json:"automatic"`
	ClassName        string     `json:"className"`
	Quantity         int        `json:"quantity"`
	StagedAt         *time.Time `json:"stagedAt"`
	FailureReason    *string    `json:"failureReason"`
	ReviewResolution *string    `json:"reviewResolution"`
	ReviewResolvedAt *time.Time `json:"reviewResolvedAt"`
	FulfilledAt      *time.Time `json:"fulfilledAt"`
	BuyerAnswer      *string    `json:"buyerAnswer"`
	CreatedAt        time.Time  `json:"createdAt"`
	// CanReview: a worker attempt under review that no person has resolved yet.
	CanReview bool `json:"canReview"`
}

func attemptSummary(a repository.ShopAttempt) shopAttemptSummaryDTO {
	return shopAttemptSummaryDTO{AttemptID: a.AttemptID, State: a.State, Automatic: a.FulfilmentMode == repository.ShopFulfilmentBuyer, ClassName: a.ClassName,
		Quantity: a.Quantity, StagedAt: a.StagedAt, FailureReason: a.FailureReason, ReviewResolution: a.ReviewResolution, ReviewResolvedAt: a.ReviewResolvedAt,
		FulfilledAt: a.FulfilledAt, BuyerAnswer: a.BuyerAnswer, CreatedAt: a.CreatedAt,
		CanReview: a.FulfilmentMode == repository.ShopFulfilmentBuyer && a.State == repository.AttemptFailedReview && a.ReviewResolution == nil}
}

func (a *App) handleShopPurchaseAttempts(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopAutoContext(w, r, true)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "purchaseID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	list, err := a.shopAttempts.AttemptsForPurchase(ctx, er.scope.OrganizationID, er.scope.InstallationID, id)
	if err != nil {
		shopFailed(w, "list delivery attempts", err)
		return
	}
	items := make([]shopAttemptSummaryDTO, 0, len(list))
	for _, at := range list {
		items = append(items, attemptSummary(at))
	}
	writeSaaSJSON(w, http.StatusOK, struct {
		Items []shopAttemptSummaryDTO `json:"items"`
	}{items})
}

// handleShopAttemptReview records what staff found in game for an order the worker stopped on, and
// resolves the review with it. NOT_SPAWNED makes the refund possible; SPAWNED makes the manual
// fulfilment possible. It never moves points and never creates a new attempt.
func (a *App) handleShopAttemptReview(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopAutoContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasShopAdminLimiter, rateLimitKey(r)) {
		return
	}
	attemptID := strings.TrimSpace(r.PathValue("attemptID"))
	var body struct {
		Outcome  string `json:"outcome"`
		Observer string `json:"observer"`
		Detail   string `json:"detail"`
	}
	if !decodeFactionBody(w, r, &body) {
		return
	}
	outcome, observer, detail := strings.ToUpper(strings.TrimSpace(body.Outcome)), strings.TrimSpace(body.Observer), strings.TrimSpace(body.Detail)
	switch {
	case attemptID == "" || len(attemptID) > 64:
		writeSaaSError(w, codeInvalidRequest, "invalid attempt id")
		return
	case outcome != repository.ReviewNotSpawned && outcome != repository.ReviewSpawned:
		writeSaaSError(w, codeInvalidRequest, "outcome must be NOT_SPAWNED or SPAWNED")
		return
	case observer == "" || len([]rune(observer)) > 64:
		writeSaaSError(w, codeInvalidRequest, "observer is required: who checked in game (1-64 characters)")
		return
	case detail == "" || len([]rune(detail)) > 500:
		writeSaaSError(w, codeInvalidRequest, "detail is required: what was found in game (1-500 characters)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	org, inst := er.scope.OrganizationID, er.scope.InstallationID
	at, err := a.shopAttempts.Get(ctx, org, inst, attemptID)
	if errors.Is(err, repository.ErrShopAttemptNotFound) {
		writeSaaSError(w, codeNotFound, "delivery attempt not found")
		return
	}
	if err != nil {
		shopFailed(w, "review delivery attempt", err)
		return
	}
	if at.FulfilmentMode != repository.ShopFulfilmentBuyer || at.State != repository.AttemptFailedReview || at.ReviewResolution != nil {
		writeSaaSError(w, codeAttemptNotReviewable, "this delivery attempt is not waiting for a review")
		return
	}
	if _, err := a.shopAttempts.RecordEvidence(ctx, org, inst, attemptID, er.user.DiscordUserID, repository.ShopAttemptEvidenceInput{
		Kind: repository.EvidenceReviewObservation, Source: repository.SourceInGameObservation, ObservedBy: observer, ObservedAt: time.Now(), Detail: detail}); err != nil {
		shopFailed(w, "review delivery attempt", err)
		return
	}
	resolved, err := a.shopAttempts.ResolveReview(ctx, org, inst, attemptID, outcome, er.user.DiscordUserID, detail)
	if errors.Is(err, repository.ErrShopAttemptStale) {
		writeSaaSError(w, codeAttemptNotReviewable, "this delivery attempt was already reviewed")
		return
	}
	if err != nil {
		shopFailed(w, "review delivery attempt", err)
		return
	}
	shopAudit("shop_delivery_attempt_reviewed", er, "attempt_id", attemptID, "outcome", outcome)
	writeSaaSJSON(w, http.StatusOK, struct {
		Attempt shopAttemptSummaryDTO `json:"attempt"`
	}{attemptSummary(resolved)})
}
