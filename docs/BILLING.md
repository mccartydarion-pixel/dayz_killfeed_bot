# Champion Billing - Stripe subscriptions, checkout, portal, webhooks (Phase 1)

The authoritative billing backend on top of the SaaS subscription foundation that already existed (`organizations`, `installations`, `subscriptions`, the 14-day internal trial, `internal/entitlements`).
Stripe is now wired in for real recurring billing, hosted Checkout, the Customer Portal and webhook reconciliation. **The website pricing page is not built here** (backend only), and **there is no
second subscription system** - Stripe state reconciles into the exact same `subscriptions` row every other SaaS surface already reads.

## 1. Audit result: what already existed

| Concern | Existing (audited on `main`) | Decision |
|---|---|---|
| Subscription table | `subscriptions`: one row per organization (`UNIQUE(organization_id)`), `plan` (free-text, default `'TRIAL'`), `status` (`TRIAL`/`ACTIVE`/`PAST_DUE`/`CANCELED`/`SUSPENDED`), `trial_ends_at`, `current_period_end`, and already-present (but always-`NULL`) `provider`/`provider_customer_id`/`provider_subscription_id` | **Reused, extended** (migration `0037`) - no parallel customer/subscription table |
| Trial | `SubscriptionRepository.EnsureTrial`, called once when an organization is created (14 days, `EnsureTrial(orgID, now+14d)`), idempotent | Reused unchanged. A Stripe trial is a *separate*, later grant (see "Trials" below) - the internal 14-day trial is what a brand-new organization has *before* it ever talks to Stripe |
| Plan names | **None approved.** `installations.plan` and `subscriptions.plan` are free-text with no enum/CHECK (by design - see `SAAS_SCHEMA.md` "Enum/status convention"); no `STARTER`/`PRO`/`PREMIUM`/etc. exist anywhere, only the placeholder value `'TRIAL'` | See "Pricing decisions" below - **PRICING DECISION REQUIRED** |
| Entitlements | `internal/entitlements.Resolve(plan)` returns the full feature set for *any* plan string - deliberately not tightened yet (its own doc comment: "a business decision for billing integration to make") | Reused unchanged; billing writes the plan key onto `subscriptions.plan` and `Resolve` picks it up automatically - no call-site changes anywhere (see "Entitlement sync") |
| Admin visibility | `GET /api/admin/subscriptions` (`adminrepo.SubscriptionRow`): plan, status, trial, period end, entitlements, install count | Extended with `billingInterval`/`cancelAtPeriodEnd` (no payment detail) |
| Website handoff docs | `docs/SAAS_API.md`'s `SubscriptionSummary` (`plan`, `status`, `trialEndsAt`, `currentPeriodEnd`) used in the dashboard/hub | Reused unchanged for those two endpoints; a **new, richer** summary (billing interval, cancellation, entitlements, `canManageBilling`) is added for the dedicated billing endpoints - see "Website handoff" |
| Environment | No `STRIPE_*` variables anywhere | Added: `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, `CHAMPION_BILLING_PLANS_JSON`, `CHAMPION_BILLING_ALLOWED_ORIGINS` (see "Environment") |

Nothing about organizations, installations, Discord configuration, factions, the economy ledger or shop purchases changes because of billing. A subscription state change touches exactly one row:
`subscriptions` for that organization.

## 2. Billing provider: Stripe, isolated

All Stripe SDK usage (`github.com/stripe/stripe-go/v82`) lives in **`internal/billing`** - no HTTP handler, no other package imports the SDK. The seam is the `billing.Provider` interface
(`EnsureCustomer`, `CreateCheckoutSession`, `CreatePortalSession`, `GetSubscription`, `ChangeSubscriptionPrice`, `SetCancelAtPeriodEnd`):

* **`StripeProvider`** (`internal/billing/stripe_provider.go`) - the real implementation.
* **`FakeProvider`** (`internal/billing/fake_provider.go`) - an in-memory implementation used by **every test in this repository** (unit and integration) and by any deployment with no
  `STRIPE_SECRET_KEY` configured. **No test ever calls the real Stripe API** - `billing.Service.HandleWebhook`'s signature verification is exercised with real HMAC signatures computed the same
  way Stripe computes them (`stripe-go`'s `webhook.ComputeSignature`), so that code path is genuinely tested without any network call.

`billing.Service` (`internal/billing/service.go`) is the orchestration layer every HTTP handler calls; it never touches SQL or the Stripe SDK directly - it calls `Store` (the subscriptions
repository) and `Provider`.

## 3. Environment variables

| Variable | Notes |
|---|---|
| `STRIPE_SECRET_KEY` | Server-only. Empty = billing runs **unconfigured**: every billing action returns `503 BILLING_UNAVAILABLE` instead of the process failing to start. Never logged, never returned by any API. |
| `STRIPE_WEBHOOK_SECRET` | Server-only, used only to verify the `Stripe-Signature` header. Empty = the webhook endpoint refuses every delivery with `ErrProviderNotConfigured`. |
| `CHAMPION_BILLING_PLANS_JSON` | The authoritative plan catalog (JSON array - see "Pricing configuration handoff"). Empty = no plan is sellable yet; the plans API returns `{"items":[]}` and checkout returns `400 INVALID_PLAN` for any key. |
| `CHAMPION_BILLING_ALLOWED_ORIGINS` | Comma-separated extra site origins (e.g. a local website dev server) a Checkout/Portal return URL may target, on top of the built-in production default `https://championshp.vip`. Never anything the client asserts about itself - see "Safe return URLs". |

No `NEXT_PUBLIC_*`-style variable is used anywhere in this backend; the plan catalog's public-safe fields are served through the API (section 6), never baked into a build.

## 4. Stripe customer & subscription mapping

**No new table.** `subscriptions` (one row per organization) gained, in migration `0037_billing_stripe`:

| Column | Notes |
|---|---|
| `provider_price_id` | The Stripe Price currently active on the subscription |
| `billing_interval` | `MONTHLY` / `YEARLY`, derived from the Stripe Price's `recurring.interval` |
| `current_period_start` | Added next to the existing `current_period_end` |
| `cancel_at_period_end` | `BOOLEAN NOT NULL DEFAULT FALSE` |
| `canceled_at` | When Stripe recorded the cancellation |
| `trial_consumed` | `BOOLEAN NOT NULL DEFAULT FALSE` - set once a `provider_subscription_id` is ever recorded; never cleared. Drives the "grant a Stripe trial once" rule (section 10) |

`provider`/`provider_customer_id`/`provider_subscription_id` already existed (`0024_saas_foundation`) and were always `NULL` until now. Indexes `idx_subscriptions_provider_customer` /
`idx_subscriptions_provider_subscription` support webhook lookups (section 9). A new table, **`billing_webhook_events`**, is the idempotency log (section 9): `(provider, event_id)` unique,
`event_type`, a best-effort `organization_id` (`ON DELETE SET NULL`) for admin debugging, `received_at`.

Stripe ids are **never** returned by any customer-facing or admin API - `SubscriptionSummaryDTO`, the dashboard/hub `SubscriptionSummary`, and `adminrepo.SubscriptionRow`/`SubscriptionInfo` all
stop at `billingInterval`/`cancelAtPeriodEnd`/`status`/`plan`/dates.

## 5. Plan catalog

`internal/billing.Plan` is the authoritative shape: `key`, `name`, `description`, `features []string`, `limits map[string]int`, `monthly`/`yearly` price metadata (amount in cents, currency, the
approved Stripe Price id), `isPublic`, `sortOrder`, `popular`, `trialDays`. `internal/billing.Catalog` is built once at startup by `LoadCatalog(CHAMPION_BILLING_PLANS_JSON)` - **no plan name,
price or feature list is hardcoded in Go**. See "Pricing configuration handoff" for the exact JSON shape and the commercial decisions required to populate it.

* **Price resolution is always server-side.** Checkout accepts a `planKey` + `interval`; the backend resolves the Stripe Price id from the catalog. A client can never submit a dollar amount or a
  Stripe price id.
* A **private** plan (`isPublic:false`) still resolves via `Catalog.Get` (so an existing subscriber or an admin script can be reconciled/reported on) but `Checkout`/`ChangePlan` refuse to sell it
  (`400 INVALID_PLAN`) - only `PublicPlans()` (what the pricing page shows) is purchasable.
* An interval a plan doesn't sell (`Price(interval) == nil`) is `400 INVALID_PLAN` too - never a fabricated price.
* `YEARLY` is fully supported by the catalog and the checkout/change-plan code paths; it is simply **not priced** until a plan's JSON includes a `yearly` block.

## 6. Public plans API

```
GET /api/saas/billing/plans
```

Any synced user (standard service-auth + acting-user chain; no organization context). Returns only public-safe fields - **no Stripe price id**:

```json
{ "items": [ { "key": "PRO", "name": "Pro", "description": "...", "features": ["killfeed","leaderboards"], "limits": {"installations":5},
  "monthly": {"amountCents":1999,"currency":"usd"}, "yearly": {"amountCents":19990,"currency":"usd"}, "popular": true, "trialDays": 14, "sortOrder": 2 } ] }
```

Private (`isPublic:false`) plans never appear. With an empty catalog, `items` is `[]`.

## 7. Current subscription

```
GET /api/saas/organizations/{organizationID}/billing/subscription
```

Any organization member (read-only). Returns `SubscriptionSummaryDTO` (section 14) - plan, status, billing interval, trial/period dates, `cancelAtPeriodEnd`, the resolved `entitlements` (from
`internal/entitlements.Resolve`, unchanged), whether a Stripe customer/subscription exists yet, and `canManageBilling` (true only for the acting user's OWNER/ADMIN role - the website uses this to
show or hide the "Manage billing" button without a second permissions call). A first read for a brand-new organization creates the internal TRIAL row exactly as the existing dashboard/hub already
did (`EnsureTrial`), so this route never 404s for a fresh organization.

## 8. Checkout

```
POST /api/saas/organizations/{organizationID}/billing/checkout
{ "planKey": "PRO", "interval": "MONTHLY", "returnPath": "/dashboard/org/5/billing?checkout=success" }
```

**Organization OWNER/ADMIN only** (`requireOrganizationRole` - the same check every other mutating SaaS route uses); a MEMBER or a faction role (even LEADER) gets `403`. Backend flow (all in
`billing.Service.Checkout`):

1. resolve the plan + price from the catalog (`400 INVALID_PLAN` if unknown/private/not sold on that interval);
2. `EnsureCustomer` - reuse the organization's existing Stripe customer id if one is already stored, otherwise create one (`Name` = the organization's name) and persist it immediately
   (`SetProviderCustomer`), so a retried checkout call never creates a second Stripe customer;
3. resolve a safe, absolute success/cancel URL (section 11);
4. decide the trial (section 10);
5. create a Stripe Checkout Session (`mode=subscription`, `client_reference_id` = the organization id, `subscription_data.metadata` carries `champion_organization_id`/`champion_plan_key`/
   `champion_billing_interval` so the *subscription* object itself - not just the Checkout Session - can be attributed by a later webhook even without a customer-id lookup);
6. return `{ "checkoutUrl": "https://checkout.stripe.com/..." }`.

Champion's own subscription/entitlement state is **not** changed by this call - only by the webhook once Stripe confirms payment (section 20).

## 9. Webhook endpoint

```
POST /api/saas/billing/webhook
```

The **one** route in this file not behind the website's `Authorization: Bearer <WEBSITE_API_SECRET>` + acting-user chain - Stripe calls it directly and never sends that header. Authentication is
the `Stripe-Signature` header, verified against `STRIPE_WEBHOOK_SECRET` (`billing.VerifyWebhookEvent` -> `webhook.ConstructEventWithOptions`, HMAC-SHA256 with a 5-minute timestamp tolerance). The
raw body is read (capped at 512 KiB) **before** any parsing, exactly as HMAC verification requires; an invalid signature is `400` and the payload is never trusted or processed.

**Events handled**: `checkout.session.completed` (`mode=subscription` only), `customer.subscription.created`, `customer.subscription.updated`, `customer.subscription.deleted`, `invoice.paid`,
`invoice.payment_failed`. Every other event type is acknowledged (`200`) and otherwise ignored - Stripe stops retrying it.

**Idempotency** (section 19 of the task): `billing_webhook_events` has `UNIQUE (provider, event_id)`. `HandleWebhook` does `INSERT ... ON CONFLICT DO NOTHING RETURNING id` **before any
processing**; if no row comes back, the event was already handled and the handler returns `200` immediately, unchanged. This is a database constraint, not a read-then-write check, so it is safe
under concurrent redelivery (Stripe retries aggressively on anything but a clean `2xx`) - proven under `-race` with 20 concurrent deliveries of the same event
(`TestConcurrentWebhookDeliveryIsRaceFree`).

**Organization attribution** (`Service.resolveOrgID`), in order: (1) the event's own `champion_organization_id` metadata (cheapest - no query), (2) a lookup by Stripe customer id
(`idx_subscriptions_provider_customer`), (3) a lookup by Stripe subscription id (`idx_subscriptions_provider_subscription`). An event that resolves to no organization is logged
(`billing_webhook_unattributed`) and acknowledged - it changes nothing and never guesses.

**A closed browser tab / an abandoned checkout never corrupts anything** (task section 20): Champion's subscription state changes *only* when a webhook (or an explicit `Reconcile`, section 18)
applies it. `Checkout` itself never writes `plan`/`status`.

## 10. Subscription status mapping

Stripe's raw status is never exposed. `billing.MapStatus`:

| Stripe | Champion |
|---|---|
| `trialing` | `TRIAL` |
| `active` | `ACTIVE` |
| `past_due`, `incomplete`, `incomplete_expired`, `unpaid` | `PAST_DUE` |
| `canceled` | `CANCELED` |
| `paused` | `SUSPENDED` |
| anything else (a future Stripe status) | `PAST_DUE` - fails toward "needs attention", never silently stays active |

`billing.MapInterval` maps a Stripe Price's `recurring.interval` (`month`/`year`) to `MONTHLY`/`YEARLY`; anything else (`week`/`day`) maps to `""` and is never written over an already-known
interval.

## 11. Safe return URLs

Checkout's `successUrl`/`cancelUrl` and the Portal's `returnUrl` are **never** built from a client-supplied host. The client may send an optional, **root-relative path only**
(`returnPath: "/dashboard/org/5/billing"` - must start with `/`, never `//...` or contain a backslash, no scheme); the origin is always chosen server-side by `billing.ResolveOrigin`:

1. the request's `Origin` header, if it exactly matches a configured allowed origin;
2. else the `Referer`'s scheme+host, if that matches;
3. else the production default, `https://championshp.vip`.

`CHAMPION_BILLING_ALLOWED_ORIGINS` adds development origins (e.g. `http://localhost:3000`) to that allowlist; the production default is always included even if the variable is unset or doesn't
mention it. A path that isn't a safe root-relative path is `400 INVALID_REQUEST` (`ErrInvalidReturnPath`).

## 12. Customer Portal

```
POST /api/saas/organizations/{organizationID}/billing/portal
{ "returnPath": "/dashboard/org/5/billing" }
```

Organization OWNER/ADMIN only. Requires an existing Stripe customer (`409 NO_BILLING_CUSTOMER` if the organization has never checked out). Returns `{ "portalUrl": "..." }`. The customer can change
their payment method, view invoices, and (if the Stripe Customer Portal configuration allows it) cancel from there too - any change made through the portal reaches Champion the same way a
checkout does: a webhook.

## 13. Upgrade, downgrade, cancel, reactivate

```
POST .../billing/plan      { "planKey": "PRO", "interval": "MONTHLY" }
POST .../billing/cancel
POST .../billing/reactivate
```

All three: organization OWNER/ADMIN only; `409 NO_ACTIVE_SUBSCRIPTION` if the organization has never completed a checkout (`provider_subscription_id` empty).

* **Upgrade and downgrade both apply immediately**, with Stripe's default proration (`ChangeSubscriptionPrice` swaps the subscription's single item to the new price; no `proration_behavior`
  override, so Stripe's default `create_prorations` applies and prorates the difference onto the next invoice). **This is a deliberate Phase 1 decision, stated explicitly rather than left
  ambiguous**: the task's stated preference was "downgrade at renewal," which in Stripe's model means introducing a *second* billing object (a Subscription Schedule with a phase boundary at the
  current period end) on top of the plain Subscription this phase already reconciles. Phase 1 ships the one-code-path version - upgrade and downgrade both go through the exact same
  `ChangeSubscriptionPrice` call - and reports the result as `plan`/`status` immediately reflecting the new price. If the product wants true downgrade-at-renewal, that is a follow-up: the DTO
  shape (a `Summary` plus, if needed, a `pendingPlanChange` field) is designed so adding it later would not break the API contract.
* **Cancel** (`Service.Cancel`) sets `cancel_at_period_end = true` via Stripe; the subscription (and every entitlement it grants) **stays fully active until the current period ends** - nothing is
  suspended or deleted immediately. `cancelAtPeriodEnd: true` is reported in the summary.
* **Reactivate** (`Service.Reactivate`) sets it back to `false`, as long as the subscription is still active (i.e. before the period actually ended and Stripe auto-canceled it - at that point
  there is no subscription left to reactivate and the next checkout starts a new one).

## 14. Entitlement sync

Champion's entitlement key set and its by-plan resolution are **unchanged** (`internal/entitlements.Resolve`). Billing's only job is to keep `subscriptions.plan` correct; `Resolve(plan)` already
returns the right feature set the moment that column changes; no billing code calls into `entitlements` beyond reading `Resolve` for the summary DTO. When the product later tightens `Resolve` to
actually differ per plan tier, **no billing call site changes** - this was true before this phase and remains true after it, by the existing package's own design.

## 15. Trials and trial-abuse prevention

The **internal** 14-day trial (`EnsureTrial`, unchanged) is what every organization has from the moment it's created, independent of Stripe. A **Stripe** trial (`subscription_data.trial_period_days`
on the Checkout Session, taken from the chosen plan's `trialDays`) is granted **at most once per organization**, gated by `trial_consumed`: the first time a `provider_subscription_id` is ever
recorded for an organization (via a webhook, not the checkout call itself - see section 9), `trial_consumed` is set and never cleared. A later checkout (the customer canceled and is
re-subscribing, or abandoned the first attempt and tries again) is `trialDays: 0` regardless of the plan's configured trial length - Stripe charges immediately.
`TestBillingCheckoutTrialGrantedOnceOnly` / `TestCheckoutGrantsTrialOnceThenNeverAgain` cover this both at the HTTP and service level.

## 16. Trial conversion / data retention

Converting from trial to paid, upgrading, downgrading, cancelling or a payment failure **only ever changes the one `subscriptions` row** for that organization. The organization, its installations,
Discord configuration, factions, Faction Hub data, the Champion Points economy ledger and Shop purchase history are never touched by anything in this phase - there is no cascade from `subscriptions`
to any of them, and nothing in `internal/billing` writes to any other table.

## 17. Payment failure handling - DECISION REQUIRED

`invoice.payment_failed` reconciles the subscription's Stripe-reported status (usually `past_due`) into Champion (`billing_payment_failed` is audited) and **does nothing else**: no installation is
suspended, no data is deleted, entitlements are unaffected (they already fully follow `entitlements.Resolve`, which does not vary by status). **What should actually happen during a `PAST_DUE`
grace period - a warning banner only, a feature downgrade, an installation suspension after N days - is an explicit product/commercial decision this phase does not make.** `PAST_DUE` is reported
faithfully in the subscription summary and the admin list so the product can decide and a future phase can act on it.

## 18. Reconciliation

`billing.Service.Reconcile(ctx, organizationID)` pulls the organization's subscription straight from Stripe (`GetSubscription`) and overwrites Champion's row with it - useful for a missed
webhook, or a future admin/cron tool. It **only ever touches the one named organization's row** (never a bulk operation across tenants) and nothing outside `subscriptions`. A subscription id
Stripe no longer recognizes (deleted outside of a webhook, e.g. from the Stripe dashboard) reconciles to `CANCELED` rather than being left stale. An organization with no Stripe subscription yet
is a no-op (nothing to reconcile against), not an error.

## 19. Plan change concurrency

Every write path (`ApplyProviderState`, `SetProviderCustomer`) is a single `UPDATE ... WHERE organization_id = $1` against the row `subscriptions.organization_id`'s own `UNIQUE` constraint already
serializes - two concurrent webhooks, or a webhook racing an admin's `ChangePlan`/`Cancel` call, for the **same** organization can never interleave into a mixed state; whichever commits last wins,
and the next reconciliation (another webhook, or an explicit `Reconcile`) settles it. Proven under `-race`: `TestConcurrentReconcileAndChangePlanAreRaceFree`,
`TestConcurrentPlanChangesOnDifferentOrganizationsAreRaceFree`, `TestConcurrentWebhooksForDifferentOrganizationsAreRaceFree`.

## 20. Security review (task section 33)

* **IDOR / cross-organization access**: every route resolves `organizationID` from the path and checks membership (or OWNER/ADMIN) before touching `subscriptions` - proven with an org-B
  OWNER acting on org A's routes (`403` everywhere) and org A's webhook events never touching org B's row (`TestBillingTenantIsolation`, `TestBillingWebhookNeverCrossesTenants`).
- **Fake plan key / fake price id**: `Checkout`/`ChangePlan` resolve the Stripe price **only** from the server-side catalog by `planKey`; nothing in the request body is ever used as a price or a
  Stripe id.
* **Redirect injection**: see section 11 - the origin is never client-supplied, only a root-relative path is accepted, validated before being appended.
* **Webhook spoofing**: HMAC-verified before any parsing; an invalid signature never reaches business logic (`TestWebhookRejectsBadSignature`, `TestBillingWebhookRejectsBadSignatureAndIsIdempotent`).
* **Duplicate webhooks**: section 9 (idempotency).
* **Privilege escalation**: checkout/portal/plan-change/cancel/reactivate all require organization OWNER/ADMIN via the same `requireOrganizationRole` every other mutating SaaS route uses; a
  faction role (including LEADER) grants nothing (`TestBillingCheckoutAuthorizationAndValidation`).
* **No card data**: Champion never collects a card number, CVC or any raw payment credential - Checkout and the Customer Portal are Stripe-hosted surfaces; this backend only ever sees ids and
  status strings.

## 21. Billing audit events

`billing_checkout_created`, `billing_subscription_activated`, `billing_subscription_updated`, `billing_subscription_cancelled`, `billing_subscription_reactivated`, `billing_payment_failed`
(plus `billing_webhook_duplicate`/`billing_webhook_unattributed`/`billing_webhook_parse_failed` for operational visibility). Every line carries `organization_id` and, for a user-initiated action,
`acting_user_id` - **never** a Stripe id, a card detail, a webhook secret or the raw webhook payload (`TestBillingAuditLogsIdsOnly` asserts this).

## 22. Admin / founder visibility

`GET /api/admin/subscriptions` (`adminrepo.SubscriptionRow`) and the organization detail/list (`adminrepo.SubscriptionInfo`) now also report `billingInterval` and `cancelAtPeriodEnd` alongside the
existing `plan`/`status`/`trialEndsAt`/`currentPeriodEnd`/`entitlements` - **no Stripe id, no payment method, no invoice detail** is ever exposed there.

## 23. Local development

1. `stripe listen --forward-to localhost:8080/api/saas/billing/webhook` (the Stripe CLI) prints a `whsec_...` value - set that as `STRIPE_WEBHOOK_SECRET`.
2. Set `STRIPE_SECRET_KEY` to a **test-mode** secret key (`sk_test_...`) from the Stripe dashboard.
3. Create test-mode Products/Prices in the Stripe dashboard (or `stripe prices create ...`) and put their ids into `CHAMPION_BILLING_PLANS_JSON` (section "Pricing configuration handoff").
4. No production secret is ever required for local development or for any test in this repository - `FakeProvider` covers every automated test, and `STRIPE_SECRET_KEY`/`STRIPE_WEBHOOK_SECRET`
   are simply left empty to run the server itself with billing reporting `BILLING_UNAVAILABLE`.

## 24. Website handoff: exact DTOs

```ts
interface BillingPrice { amountCents: number; currency: string }   // no Stripe price id
interface BillingPlan {
  key: string; name: string; description: string; features: string[]; limits: Record<string, number>;
  monthly: BillingPrice | null; yearly: BillingPrice | null;   // null = not sold on that interval
  popular: boolean; trialDays: number; sortOrder: number;
}
interface BillingPlansResponse { items: BillingPlan[] }        // GET /api/saas/billing/plans

interface SubscriptionSummary {                                 // GET .../billing/subscription
  plan: string; status: "TRIAL" | "ACTIVE" | "PAST_DUE" | "CANCELED" | "SUSPENDED";
  billingInterval: "MONTHLY" | "YEARLY" | null;
  trialEndsAt: string | null; currentPeriodStart: string | null; currentPeriodEnd: string | null;   // RFC 3339
  cancelAtPeriodEnd: boolean;
  entitlements: string[];              // internal/entitlements keys
  hasBillingCustomer: boolean;         // a Portal session can be created
  hasActiveSubscription: boolean;      // checkout has completed at least once - plan/cancel/reactivate apply
  canManageBilling: boolean;           // the acting user is this organization's OWNER/ADMIN
}

interface CheckoutRequest { planKey: string; interval: "MONTHLY" | "YEARLY"; returnPath?: string }   // POST .../billing/checkout
interface CheckoutResponse { checkoutUrl: string }

interface PortalRequest { returnPath?: string }               // POST .../billing/portal
interface PortalResponse { portalUrl: string }

interface ChangePlanRequest { planKey: string; interval: "MONTHLY" | "YEARLY" }   // POST .../billing/plan -> SubscriptionSummary
// POST .../billing/cancel and POST .../billing/reactivate take no body -> SubscriptionSummary
```

Error codes (standard envelope): `INVALID_PLAN` (400), `NO_ACTIVE_SUBSCRIPTION` (409), `NO_BILLING_CUSTOMER` (409), `BILLING_UNAVAILABLE` (503, billing not configured on this environment),
`INVALID_REQUEST` (400, e.g. a bad `returnPath`), `FORBIDDEN` (403, not OWNER/ADMIN), `UNAUTHORIZED` (401), `RATE_LIMITED` (429). Rate limit: 20 billing actions/minute per acting user
(checkout/portal/plan/cancel/reactivate combined); reads are unlimited.

UI notes: treat `plan`/`status`/`billingInterval` as open sets; show "Link billing" / a Checkout button when `hasBillingCustomer` is false; hide manage-billing controls entirely when
`canManageBilling` is false (the API also enforces this - the flag only saves an extra failed call); show a "Renews on `currentPeriodEnd`, will not renew" banner when `cancelAtPeriodEnd` is true
and offer Reactivate.

## 25. Pricing configuration handoff

**No commercial plan name, price, feature list, limit, trial length or "popular" flag is approved yet** - none existed anywhere in the codebase before this phase, and none was invented here
(task section 2 / 42: "Do NOT invent production prices"). `internal/billing.LoadCatalog` and the whole plan-catalog/checkout/entitlement pipeline are fully built and tested against a realistic
sample catalog; production simply needs `CHAMPION_BILLING_PLANS_JSON` populated once those decisions are made. Exact shape (one entry per plan; `monthly`/`yearly` omitted entirely = not sold on
that interval):

```json
[
  {
    "key": "PRO",
    "name": "Pro",
    "description": "For a growing community.",
    "features": ["killfeed", "leaderboards", "live_players", "advanced_stats"],
    "limits": { "installations": 5 },
    "monthly": { "amountCents": 1999, "currency": "usd", "stripePriceId": "price_..." },
    "yearly":  { "amountCents": 19990, "currency": "usd", "stripePriceId": "price_..." },
    "isPublic": true,
    "sortOrder": 2,
    "popular": true,
    "trialDays": 14
  }
]
```

**Commercial decisions required before this goes live:**

1. Approved plan names/keys (e.g. is it `STARTER`/`PRO`/`ELITE`, or something else entirely?).
2. Monthly price per plan.
3. Annual price per plan, if annual billing is offered at launch (fully supported; simply omit `yearly` per plan until decided - never a fabricated discount).
4. The feature list and any numeric limits (e.g. installation count) per plan - and, separately, when `internal/entitlements.Resolve` should start actually differing by plan (today it returns the
   full set for every plan, by pre-existing design).
5. Trial length per plan (`trialDays`) - may be `0`.
6. Which plan (if any) is the "popular"/recommended one.
7. The real Stripe Product/Price ids for each plan+interval (created in the Stripe dashboard or via the Stripe CLI/API, in test mode first).
8. The payment-failure / grace-period policy (section 17): what should actually happen while a subscription is `PAST_DUE`.
9. Whether the "downgrade at renewal" behaviour (task's stated preference) is required for launch, or Phase 1's "both directions apply immediately with proration" (section 13) is acceptable.

## 26. Tests

* `internal/billing/*_test.go` (unit, no network, no database): plan catalog parsing/validation, status/interval mapping, safe-redirect origin resolution, the full `Service` (checkout
  authorization boundary is HTTP-layer and tested there; everything else - unknown/private/unsold plans, trial-once, customer reuse, change-plan/cancel/reactivate state machine, webhook
  signature verification with real HMAC signatures, webhook idempotency, tenant-attribution fallbacks, reconciliation including a subscription Stripe no longer knows) against `FakeProvider`
  and an in-memory `Store`; four `-race` tests for concurrent webhook delivery/reconciliation/plan changes.
* `internal/app/saas_api_billing_integration_test.go` (real routes + real PostgreSQL, `FakeProvider` wired into the real `App`): plans, subscription summary (incl. `canManageBilling` by role),
  checkout authorization and validation (unknown/private/unsold plan, unsafe return paths), trial-once through the real webhook endpoint, webhook signature rejection and idempotency against the
  real HTTP endpoint, tenant isolation (webhooks and API calls), the portal, upgrade/downgrade/cancel/reactivate, rate limits, audit log content, and admin visibility.
* Existing subscription/trial/dashboard/hub tests (`TestHubSummaryNoSecrets`, `TestHubCrossTenantRejected`, etc.) pass unchanged.
