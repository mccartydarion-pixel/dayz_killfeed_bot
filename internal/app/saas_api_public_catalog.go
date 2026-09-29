package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/casebilling"
)

// Public pricing catalog (Phase 6.26I). The website renders it for anonymous visitors, so this
// route needs only the website's server-to-server bearer (requireSaaSServiceAuth) - never a
// Discord login or a synced acting user. The authenticated GET /api/saas/billing/plans and every
// checkout, subscription and entitlement route are unchanged.
//
// Every value is derived from backend configuration: the base catalog (CHAMPION_BILLING_PLANS_JSON,
// public plans only) and the C.A.S.E. catalog whose purchasability already requires the sales flag,
// a configured Stripe price and a verified tier. No Stripe, customer, subscription or organization
// identifier is ever included.

const (
	publicCatalogVersion      = 1
	publicCatalogCacheControl = "public, max-age=300, stale-while-revalidate=600"

	publicPlanAvailable   = "AVAILABLE"   // purchasable now
	publicPlanUnavailable = "UNAVAILABLE" // listed, but checkout is not configured or no price is sold
	publicPlanComingSoon  = "COMING_SOON" // C.A.S.E. add-on whose release gates are closed

	publicCategoryBase = "BASE"
	publicCategoryCase = "CASE_ADDON"
)

type publicPriceDTO struct {
	Interval    string `json:"interval"` // MONTHLY | YEARLY
	AmountCents int64  `json:"amountCents"`
	Currency    string `json:"currency"`
}

type publicPlanDTO struct {
	Key              string           `json:"key"`
	Category         string           `json:"category"`
	Name             string           `json:"name"`
	DisplayTier      string           `json:"displayTier"`
	Description      string           `json:"description"`
	Features         []string         `json:"features"`
	Prices           []publicPriceDTO `json:"prices"`
	Availability     string           `json:"availability"`
	Purchasable      bool             `json:"purchasable"`
	Popular          bool             `json:"popular"`
	SortOrder        int              `json:"sortOrder"`
	PerServer        bool             `json:"perServer"`
	RequiresBasePlan bool             `json:"requiresBasePlan"`
}

type publicCatalogResponse struct {
	Version int             `json:"version"`
	Plans   []publicPlanDTO `json:"plans"`
}

func (a *App) registerPublicCatalogRoute() {
	if a.saasPublicCatalogLimiter == nil {
		// Keyed by rateLimitKey: without an acting user that is the caller's address, i.e. the
		// website server itself, so the budget is generous and the response is cacheable.
		a.saasPublicCatalogLimiter = newSaaSRateLimiter(time.Minute, 120)
	}
	a.HTTPServer.Handle("GET /api/saas/billing/public-plans", a.handlePublicCatalog)
}

// buildPublicCatalog returns nil when pricing configuration is unavailable (fail safe: the caller
// answers 503 and never invents prices).
func buildPublicCatalog(svc *billing.Service) *publicCatalogResponse {
	if svc == nil {
		return nil
	}
	base := svc.Plans()
	if len(base) == 0 {
		return nil
	}
	checkout := svc.CheckoutConfigured()
	out := &publicCatalogResponse{Version: publicCatalogVersion, Plans: []publicPlanDTO{}}
	for _, p := range base {
		prices := []publicPriceDTO{}
		for _, iv := range []struct {
			name string
			meta *billing.PriceMeta
		}{{"MONTHLY", p.Monthly}, {"YEARLY", p.Yearly}} {
			if iv.meta != nil && iv.meta.AmountCents > 0 {
				prices = append(prices, publicPriceDTO{Interval: iv.name, AmountCents: iv.meta.AmountCents, Currency: strings.ToLower(iv.meta.Currency)})
			}
		}
		purchasable := checkout && len(prices) > 0
		availability := publicPlanUnavailable
		if purchasable {
			availability = publicPlanAvailable
		}
		out.Plans = append(out.Plans, publicPlanDTO{
			Key: p.Key, Category: publicCategoryBase, Name: p.Name, DisplayTier: strings.ToUpper(p.Key),
			Description: p.Description, Features: append([]string{}, p.Features...), Prices: prices,
			Availability: availability, Purchasable: purchasable, Popular: p.Popular, SortOrder: p.SortOrder,
		})
	}
	maxBase := 0
	for _, p := range out.Plans {
		if p.SortOrder > maxBase {
			maxBase = p.SortOrder
		}
	}
	for i, p := range svc.CasePlans() {
		// Command stays off the public catalog until its release gates pass (its tier is verified,
		// priced and sales are enabled); Watch and Pro are always listed, purchasable only when open.
		if p.Tier == casebilling.Command && !p.Purchasable {
			continue
		}
		availability := publicPlanComingSoon
		if p.Purchasable {
			availability = publicPlanAvailable
		}
		interval := "MONTHLY"
		if !strings.EqualFold(p.Interval, "month") {
			interval = strings.ToUpper(p.Interval)
		}
		out.Plans = append(out.Plans, publicPlanDTO{
			Key: string(p.Tier), Category: publicCategoryCase, Name: p.Name,
			DisplayTier: strings.TrimPrefix(string(p.Tier), "CASE_"), Description: p.Description,
			Features:     append([]string{}, p.Features...),
			Prices:       []publicPriceDTO{{Interval: interval, AmountCents: p.AmountCents, Currency: strings.ToLower(p.Currency)}},
			Availability: availability, Purchasable: p.Purchasable, SortOrder: maxBase + 100 + i,
			PerServer: true, RequiresBasePlan: true,
		})
	}
	sort.SliceStable(out.Plans, func(i, j int) bool { return out.Plans[i].SortOrder < out.Plans[j].SortOrder })
	return out
}

// handlePublicCatalog is GET /api/saas/billing/public-plans.
func (a *App) handlePublicCatalog(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	if !enforceRateLimit(w, a.saasPublicCatalogLimiter, rateLimitKey(r)) {
		return
	}
	catalog := buildPublicCatalog(a.Billing)
	if catalog == nil {
		w.Header().Set("Cache-Control", "no-store")
		writeSaaSError(w, codeBillingUnavailable, "pricing is temporarily unavailable")
		return
	}
	body, err := json.Marshal(catalog)
	if err != nil {
		writeSaaSError(w, codeInternalError, "pricing is temporarily unavailable")
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("Cache-Control", publicCatalogCacheControl)
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
