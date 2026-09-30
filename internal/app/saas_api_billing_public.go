package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"unicode"

	"github.com/yourname/dayz-killfeed/internal/billing"
)

// --- public catalog --------------------------------------------------------------------------------
//
// GET /api/saas/billing/public-plans serves the website's public pricing page and homepage price
// teaser, which render for anonymous visitors. It therefore requires only the service bearer token
// (requireSaaSServiceAuth), never an acting user, and exposes nothing beyond what
// GET /api/saas/billing/plans already shows a signed-in user: the IsPublic catalog, with no Stripe
// price ids. The shape is the website's PublicCatalogV1 (lib/saas/publicCatalog.ts).

const publicCatalogVersion = 1

// publicCatalogCacheControl lets the website cache the catalog briefly and revalidate with ETag.
const publicCatalogCacheControl = "public, max-age=300"

type publicCatalogPriceDTO struct {
	Interval    string `json:"interval"` // MONTHLY | YEARLY
	AmountCents int64  `json:"amountCents"`
	Currency    string `json:"currency"`
}

type publicCatalogPlanDTO struct {
	Key              string                  `json:"key"`
	Category         string                  `json:"category"` // BASE | CASE_ADDON
	Name             string                  `json:"name"`
	DisplayTier      string                  `json:"displayTier"`
	Description      string                  `json:"description"`
	Features         []string                `json:"features"`
	Prices           []publicCatalogPriceDTO `json:"prices"`
	Availability     string                  `json:"availability"` // AVAILABLE | UNAVAILABLE | COMING_SOON
	Purchasable      bool                    `json:"purchasable"`
	Popular          bool                    `json:"popular"`
	SortOrder        int                     `json:"sortOrder"`
	PerServer        bool                    `json:"perServer"`
	RequiresBasePlan bool                    `json:"requiresBasePlan"`
}

type publicCatalogResponse struct {
	Version int                    `json:"version"`
	Plans   []publicCatalogPlanDTO `json:"plans"`
}

// publicCatalogPlanFromPlan maps one catalog plan to the public DTO. Every catalog plan today is a
// base Champion plan sold per organization; C.A.S.E. add-ons are not in this catalog.
func publicCatalogPlanFromPlan(p billing.Plan) publicCatalogPlanDTO {
	prices := make([]publicCatalogPriceDTO, 0, 2)
	if p.Monthly != nil {
		prices = append(prices, publicCatalogPriceDTO{Interval: "MONTHLY", AmountCents: p.Monthly.AmountCents, Currency: p.Monthly.Currency})
	}
	if p.Yearly != nil {
		prices = append(prices, publicCatalogPriceDTO{Interval: "YEARLY", AmountCents: p.Yearly.AmountCents, Currency: p.Yearly.Currency})
	}
	return publicCatalogPlanDTO{
		Key:              p.Key,
		Category:         "BASE",
		Name:             p.Name,
		DisplayTier:      displayTierFromKey(p.Key),
		Description:      p.Description,
		Features:         append([]string{}, p.Features...),
		Prices:           prices,
		Availability:     "AVAILABLE",
		Purchasable:      len(prices) > 0,
		Popular:          p.Popular,
		SortOrder:        p.SortOrder,
		PerServer:        false,
		RequiresBasePlan: false,
	}
}

// displayTierFromKey turns a catalog key like "LOW" or "CASE_COMMAND" into "Low" / "Case Command".
func displayTierFromKey(key string) string {
	words := strings.FieldsFunc(strings.TrimSpace(key), func(r rune) bool { return r == '_' || r == '-' || unicode.IsSpace(r) })
	for i, w := range words {
		lower := strings.ToLower(w)
		runes := []rune(lower)
		runes[0] = unicode.ToUpper(runes[0])
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}

// handleBillingPublicPlans is GET /api/saas/billing/public-plans: service auth only, no acting user.
func (a *App) handleBillingPublicPlans(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	if a.Billing == nil {
		writeSaaSError(w, codeBillingUnavailable, "billing unavailable")
		return
	}
	plans := a.Billing.Plans()
	resp := publicCatalogResponse{Version: publicCatalogVersion, Plans: make([]publicCatalogPlanDTO, 0, len(plans))}
	for _, p := range plans {
		resp.Plans = append(resp.Plans, publicCatalogPlanFromPlan(p))
	}
	body, err := json.Marshal(resp)
	if err != nil {
		writeSaaSError(w, codeInternalError, "encode public catalog")
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("Cache-Control", publicCatalogCacheControl)
	w.Header().Set("ETag", etag)
	if strings.TrimSpace(r.Header.Get("If-None-Match")) == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
