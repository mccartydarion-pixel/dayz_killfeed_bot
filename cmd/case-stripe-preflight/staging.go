package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/billing"
)

// requiredWebhookEvents are exactly the events Champion's webhook handles. The
// refund/dispute/void events (Phase 6.26B) reconcile C.A.S.E. invoice coverage; without
// them a refunded or disputed add-on invoice would keep granting access.
var requiredWebhookEvents = []string{
	"checkout.session.completed", "customer.subscription.created", "customer.subscription.updated",
	"customer.subscription.deleted", "invoice.paid", "invoice.payment_failed",
	"charge.refunded", "charge.refund.updated",
	"charge.dispute.created", "charge.dispute.updated", "charge.dispute.closed", "charge.dispute.funds_reinstated",
	"invoice.voided", "invoice.marked_uncollectible",
}

// stripeGet performs one authenticated read-only GET; errors never include
// the key or the response body.
func stripeGet(ctx context.Context, client *http.Client, baseURL, key, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return 0, errors.New("could not construct read-only Stripe request")
	}
	req.SetBasicAuth(key, "")
	resp, err := client.Do(req)
	if err != nil {
		return 0, errors.New("read-only Stripe GET failed; verify test key and network access")
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK || readErr != nil {
		return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if json.Unmarshal(body, out) != nil {
		return resp.StatusCode, errors.New("Stripe response could not be decoded")
	}
	return resp.StatusCode, nil
}

// verifyBaseCatalog proves every base plan price in the staging catalog
// exists in the SAME sandbox as the key. Without that, no staging
// organization can hold a paid base plan and every C.A.S.E. checkout is
// refused (CASE_BASE_REQUIRED). A base price that is really a C.A.S.E.
// product, or is priced differently from the catalog, is refused too.
func verifyBaseCatalog(ctx context.Context, client *http.Client, baseURL, key, catalogJSON string, caseSpecs []expectedPrice, output io.Writer) error {
	catalog, err := billing.LoadCatalog(catalogJSON)
	if err != nil {
		return errors.New("CHAMPION_BILLING_PLANS_JSON is not a valid catalog")
	}
	if catalog.Empty() {
		return errors.New("CHAMPION_BILLING_PLANS_JSON has no plans; staging needs a paid base plan before C.A.S.E. checkout")
	}
	caseIDs := map[string]bool{}
	for _, s := range caseSpecs {
		caseIDs[s.priceID] = true
	}
	checked := 0
	for _, plan := range catalog.Plans() {
		for _, iv := range []struct {
			interval string
			meta     *billing.PriceMeta
		}{{"month", plan.Monthly}, {"year", plan.Yearly}} {
			meta := iv.meta
			if meta == nil {
				continue
			}
			if caseIDs[meta.StripePriceID] {
				return fmt.Errorf("base plan %s reuses a C.A.S.E. price", plan.Key)
			}
			var p stripePrice
			status, err := stripeGet(ctx, client, baseURL, key, "/v1/prices/"+url.PathEscape(meta.StripePriceID)+"?expand%5B%5D=product", &p)
			if err != nil {
				if status == http.StatusNotFound {
					return fmt.Errorf("base plan %s %s price %s does not exist in this sandbox; create sandbox base prices and update the staging catalog", plan.Key, iv.interval, meta.StripePriceID)
				}
				return fmt.Errorf("base plan %s price lookup failed: %v", plan.Key, err)
			}
			if !p.Active || p.Livemode == nil || *p.Livemode || p.UnitAmount == nil || *p.UnitAmount != meta.AmountCents ||
				!strings.EqualFold(p.Currency, meta.Currency) || p.Recurring == nil || p.Recurring.Interval != iv.interval ||
				p.Recurring.IntervalCount != 1 || p.Product.Metadata["champion_product_kind"] == "CASE_ADDON" {
				return fmt.Errorf("base plan %s %s price does not match the catalog (active, test mode, amount, interval, non-C.A.S.E. product)", plan.Key, iv.interval)
			}
			checked++
			fmt.Fprintf(output, "PASS BASE %s: %s USD %d cents/%s, active, livemode=false\n", plan.Key, meta.StripePriceID, meta.AmountCents, iv.interval)
		}
	}
	if checked == 0 {
		return errors.New("base catalog has no priced plans")
	}
	return nil
}

type webhookEndpoint struct {
	ID            string   `json:"id"`
	URL           string   `json:"url"`
	Status        string   `json:"status"`
	Livemode      *bool    `json:"livemode"`
	EnabledEvents []string `json:"enabled_events"`
}

// verifyWebhookEndpoint checks that the sandbox has an ENABLED test-mode
// endpoint for exactly this staging URL, subscribed to every event Champion
// handles. The signing secret is never readable here; the first delivered
// event proves it (see the runbook).
func verifyWebhookEndpoint(ctx context.Context, client *http.Client, baseURL, key, wantURL string, output io.Writer) error {
	u, err := url.Parse(wantURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "/api/saas/billing/webhook" {
		return errors.New("webhook URL must be https://<staging-host>/api/saas/billing/webhook")
	}
	var list struct {
		Data []webhookEndpoint `json:"data"`
	}
	if _, err := stripeGet(ctx, client, baseURL, key, "/v1/webhook_endpoints?limit=100", &list); err != nil {
		return fmt.Errorf("webhook endpoint list failed (%v); a restricted key needs Webhook Endpoints: read", err)
	}
	for _, ep := range list.Data {
		if ep.URL != wantURL {
			continue
		}
		if ep.Status != "enabled" || ep.Livemode == nil || *ep.Livemode {
			return errors.New("staging webhook endpoint exists but is disabled or not test mode")
		}
		have := map[string]bool{}
		for _, e := range ep.EnabledEvents {
			have[e] = true
		}
		var missing []string
		for _, e := range requiredWebhookEvents {
			if !have[e] && !have["*"] {
				missing = append(missing, e)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("staging webhook endpoint is missing events: %s", strings.Join(missing, ", "))
		}
		fmt.Fprintf(output, "PASS WEBHOOK: %s enabled, livemode=false, all %d required events\n", ep.ID, len(requiredWebhookEvents))
		return nil
	}
	return errors.New("no sandbox webhook endpoint for the staging URL; create it before enabling checkout")
}
