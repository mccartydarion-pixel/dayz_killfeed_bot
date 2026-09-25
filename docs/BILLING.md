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
show or hide the "Manage billing" button without a second permissions call). Onboarding V2 adds `intendedPlan`, `trialStatus`, `trialDaysRemaining` and `billingRequired` (section 28).
Reading never grants a trial: an organization with no subscription row reads as `plan: "NONE"`, `status: "INACTIVE"`, `trialStatus: "NOT_STARTED"`, `billingRequired: true`, and nothing is
written. The route never 404s for a fresh organization.

## 8. Checkout

```
POST /api/saas/organizations/{organizationID}/billing/checkout
{ "planKey": "PRO", "interval": "MONTHLY", "returnPath": "/dashboard/subscription?checkout=success" }
```

**Organization OWNER/ADMIN only** (`requireOrganizationRole` - the same check every other mutating SaaS route uses); a MEMBER or a faction role (even LEADER) gets `403`. Backend flow (all in
`billing.Service.Checkout`):

1. resolve the plan + price from the catalog (`400 INVALID_PLAN` if unknown/private/not sold on that interval);
2. `EnsureCustomer` - reuse the organization's existing Stripe customer id if one is already stored, otherwise create one (`Name` = the organization's name) and persist it immediately
   (`SetProviderCustomer`), so a retried checkout call never creates a second Stripe customer;
3. resolve a safe, absolute success/cancel URL (section 11);
4. no trial: `trialDays` is always 0 (section 28) - the customer's one trial is the no-card trial, which needs no Stripe object. An organization without a subscription row
   gets an `INACTIVE` row first (never a `TRIAL` one), for the Stripe ids to land on;
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

**Organization attribution** (`Service.resolveOrgID`) requires a binding Champion itself authored: (1) the `champion_organization_id` metadata Champion sets server-side on its
Checkout Session's subscription (for invoices, the copy Stripe places on `parent.subscription_details.metadata`), or (2) the Stripe subscription id already stored on that
organization's row (`idx_subscriptions_provider_subscription`). **A Stripe customer id alone never attributes an event** (changed in Phase 6.19.1): a subscription created outside
Champion - for example from the Stripe Dashboard - on an organization's customer would otherwise overwrite that organization's base subscription. The customer id is a consistency
check only: when the organization has a saved customer that differs from the event's, the event is refused (`reason=customer_mismatch`), and metadata that conflicts with the stored
subscription is refused too. `checkout.session.completed` additionally requires `client_reference_id`; its `champion_organization_id` metadata (when present) must name the same
organization and its customer must match the saved one. An event that resolves to no organization is logged (`billing_webhook_unattributed`, with a `reason`) and acknowledged -
it changes nothing and never guesses. A session created outside Champion (no `client_reference_id`, empty metadata) is never bound to an organization, even if it was paid: pay
the Champion-created session instead, and cancel the foreign subscription in Stripe.

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
(`returnPath: "/dashboard/subscription"` - must start with `/`, never `//...` or contain a backslash, no scheme); the origin is always chosen server-side by `billing.ResolveOrigin`:

1. the request's `Origin` header, if it exactly matches a configured allowed origin;
2. else the `Referer`'s scheme+host, if that matches;
3. else the production default, `https://championshp.vip`.

`CHAMPION_BILLING_ALLOWED_ORIGINS` adds development origins (e.g. `http://localhost:3000`) to that allowlist; the production default is always included even if the variable is unset or doesn't
mention it. A path that isn't a safe root-relative path is `400 INVALID_REQUEST` (`ErrInvalidReturnPath`).

**Default routes** (used whenever a caller sends no `returnPath` at all): success `/dashboard/subscription?checkout=success`, cancel `/dashboard/subscription?checkout=cancelled`, portal return
`/dashboard/subscription` - the current Champion website route. Nothing in this backend ever points at `/billing` (a retired page that no longer exists on the website; pointing Checkout's
`cancel_url` at it caused a production 404 on cancellation).

**Checkout's cancel URL is derived from the caller's `returnPath`, not from an unrelated default.** The website contract sends one `returnPath` for the *success* case
(`/dashboard/subscription?checkout=success`); `billing.DeriveCancelPath` takes that same validated root-relative path and query, overwrites the `checkout` query parameter to `cancelled` via
`net/url` (never string substitution), and leaves every other query parameter untouched - so cancelling always lands back on the same billing page the success redirect would have, with
`checkout=cancelled` instead of `checkout=success`. Only when the caller sends no `returnPath` at all does Checkout fall back to its own configured `cancelPath` default above. The Customer Portal
is unaffected by this - it has always taken its own single `returnPath` (or its own default) directly, with no success/cancel split.

## 12. Customer Portal

```
POST /api/saas/organizations/{organizationID}/billing/portal
{ "returnPath": "/dashboard/subscription" }
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

**Superseded by Customer Onboarding V2 (section 28).** There is exactly one trial: the no-card 14-day trial, one per Discord account (`trial_grants`), started with no payment method and no
Stripe object. Checkout never requests a Stripe trial (`trialDays: 0` always), so `trial_consumed` no longer gates anything; it is still set the first time a `provider_subscription_id` is
recorded. Creating more organizations never mints more trials. `TestCheckoutNeverRequestsAStripeTrial` / `TestBillingCheckoutNeverGrantsAStripeTrial` and the tests in
`internal/billing/trial_test.go` / `internal/app/saas_api_trial_integration_test.go` cover this.

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
  plan: string; status: "TRIAL" | "ACTIVE" | "PAST_DUE" | "CANCELED" | "SUSPENDED" | "INACTIVE";
  billingInterval: "MONTHLY" | "YEARLY" | null;
  trialEndsAt: string | null; currentPeriodStart: string | null; currentPeriodEnd: string | null;   // RFC 3339
  cancelAtPeriodEnd: boolean;
  entitlements: string[];              // internal/entitlements keys
  hasBillingCustomer: boolean;         // a Portal session can be created
  hasActiveSubscription: boolean;      // checkout has completed at least once - plan/cancel/reactivate apply
  canManageBilling: boolean;           // the acting user is this organization's OWNER/ADMIN
  intendedPlan: string | null;         // Onboarding V2: plan picked before paying (a preference only)
  trialStatus: "NOT_STARTED" | "ACTIVE" | "EXPIRED" | "NOT_ELIGIBLE" | "CONVERTED";
  trialDaysRemaining: number;          // 0 unless trialStatus is ACTIVE
  billingRequired: boolean;            // true = activate a paid plan to set up a (new) service
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

**Phase 1.2 (2026-09-22): the LOW/MEDIUM/HIGH catalog below is commercially approved and active.** It is the exact, verbatim value of `CHAMPION_BILLING_PLANS_JSON` - `internal/billing.LoadCatalog`
takes this JSON as-is, nothing is hardcoded into Go (`internal/billing/pricing_catalog_test.go` / `internal/app/saas_api_billing_integration_test.go`'s `TestApprovedPricingCatalog*` tests assert
against this exact catalog). All three plans currently sell **monthly only** - no annual price has been approved, so `yearly` is omitted for every plan and `POST .../billing/checkout` with
`"interval":"YEARLY"` returns `400 INVALID_PLAN` for all three (nothing here fabricates a discount). `limits.maxSlots` is plan metadata only in this phase - **no automatic Nitrado slot-count
enforcement exists yet**; `limits.installations` stays `1` for every plan, matching Phase 1's existing (unchanged) entitlement behavior.

The three Stripe Price ids below are **configured identifiers only** - this catalog does not assert, and Champion's code has no way to assert, that they are test-mode vs. live-mode from their
format alone. Which Stripe account/mode a checkout actually talks to is determined entirely by which `STRIPE_SECRET_KEY` is configured on the running environment (section 23 "Local development"),
never by the price id's shape. As of Phase 1.2, Railway is intentionally left on a **test-mode** `STRIPE_SECRET_KEY`/`STRIPE_WEBHOOK_SECRET` - Champion has **not** been switched to live Stripe
billing.

```json
[
  {
    "key": "LOW",
    "name": "Low Tier",
    "description": "For smaller DayZ communities with up to 32 player slots.",
    "features": ["Killfeed", "Faction Hub", "Leaderboards", "Champion Points Economy", "Champion Shop", "Embed Designer", "Discord Integration", "Nitrado Integration"],
    "limits": { "installations": 1, "maxSlots": 32 },
    "monthly": { "amountCents": 599, "currency": "usd", "stripePriceId": "price_1UIQiD65uHRSytQgoMRICl5h" },
    "isPublic": true,
    "sortOrder": 1,
    "popular": false,
    "trialDays": 7
  },
  {
    "key": "MEDIUM",
    "name": "Medium Tier",
    "description": "For growing DayZ communities with 33 to 64 player slots.",
    "features": ["Killfeed", "Faction Hub", "Leaderboards", "Champion Points Economy", "Champion Shop", "Embed Designer", "Discord Integration", "Nitrado Integration"],
    "limits": { "installations": 1, "maxSlots": 64 },
    "monthly": { "amountCents": 999, "currency": "usd", "stripePriceId": "price_1UIQiD65uHRSytQghSQQOVpG" },
    "isPublic": true,
    "sortOrder": 2,
    "popular": true,
    "trialDays": 7
  },
  {
    "key": "HIGH",
    "name": "High Tier",
    "description": "For large DayZ communities with 65 to 128 player slots.",
    "features": ["Killfeed", "Faction Hub", "Leaderboards", "Champion Points Economy", "Champion Shop", "Embed Designer", "Discord Integration", "Nitrado Integration"],
    "limits": { "installations": 1, "maxSlots": 128 },
    "monthly": { "amountCents": 1499, "currency": "usd", "stripePriceId": "price_1UIQiD65uHRSytQgydtA4Pzj" },
    "isPublic": true,
    "sortOrder": 3,
    "popular": false,
    "trialDays": 7
  }
]
```

To activate on Railway: set `CHAMPION_BILLING_PLANS_JSON` to the one-line minified form of the JSON above (whitespace doesn't matter to `LoadCatalog`, but the value must be valid JSON on a single
env var). **Never put `STRIPE_SECRET_KEY` or `STRIPE_WEBHOOK_SECRET` in this file or any other doc** - those stay server-only secrets, set directly in Railway's environment settings.

### Trial interaction (superseded by Onboarding V2)

Phase 1.2 flagged that the internal 14-day trial and a per-plan Stripe trial (`trialDays: 7`) stacked: a checkout
replaced whatever was left of the internal trial with a fresh Stripe trial. **Customer Onboarding V2 resolves this
(section 28): there is one trial, the no-card 14-day trial, and every checkout is billed immediately (Stripe trial days
are always 0, whatever `trialDays` the catalog JSON carries).** `LoadCatalog` still accepts and validates `trialDays`
(so the existing Railway value keeps parsing) but never uses it; the plans API reports `trialDays: 0`. Removing the
field from `CHAMPION_BILLING_PLANS_JSON` is optional and changes nothing. Prices are unchanged: LOW $5.99, MEDIUM $9.99,
HIGH $14.99 per month.

**Commercial decisions still required before Champion goes fully live:**

1. ~~Approved plan names/keys~~ - **decided (Phase 1.2): `LOW`, `MEDIUM`, `HIGH`.**
2. ~~Monthly price per plan~~ - **decided (Phase 1.2): $5.99 / $9.99 / $14.99.**
3. Annual price per plan, if annual billing is offered at all - **still undecided; `yearly` stays omitted for all three plans until a real annual price is approved (never a fabricated discount).**
4. ~~The feature list and any numeric limits per plan~~ - **decided (Phase 1.2): the 8-feature list and `maxSlots`/`installations` above.** When `internal/entitlements.Resolve` should start
   actually differing by plan (today it returns the full set for every plan, by pre-existing design) is still undecided.
5. ~~Trial length per plan~~ - **decided (Phase 1.2): 7 days for all three plans.**
6. ~~Which plan is "popular"~~ - **decided (Phase 1.2): `MEDIUM`.**
7. ~~The real Stripe Product/Price ids~~ - **decided (Phase 1.2), test-mode: see the catalog above. Live-mode ids are a separate, later decision - "do not switch Champion to live Stripe billing
   yet".**
8. The payment-failure / grace-period policy (section 17): what should actually happen while a subscription is `PAST_DUE` - **still undecided.**
9. Whether the "downgrade at renewal" behaviour is required for launch, or Phase 1's "both directions apply immediately with proration" (section 13) is acceptable - **still undecided.**
10. *(New, surfaced by Phase 1.2)* How the internal 14-day organization trial and the new Stripe 7-day plan trial should interact, if at all, beyond "the later one silently overwrites the earlier
    one" as described above - **still undecided.**
11. Automatic Nitrado slot-count enforcement from `limits.maxSlots` - **explicitly out of scope for this phase; still undecided whether/when it should exist.**

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
* `internal/billing/trial_test.go` and `internal/app/saas_api_trial_integration_test.go` (Onboarding V2, section 28): no-card trial start, idempotency, no clock restart,
  one trial per account across organizations (including under concurrency), expired trial, paid rows never overwritten, the installation limit (including under concurrency),
  `BILLING_REQUIRED`, and the 0047 backfill.
* Existing subscription/trial/dashboard/hub tests (`TestHubSummaryNoSecrets`, `TestHubCrossTenantRejected`, etc.) pass unchanged.

## 27. Payment/invoice history (Champion Access Model Phase 2, Part D)

Before this phase, Champion persisted **only current subscription state** (`subscriptions`) - no
historical record of individual payments or failures existed anywhere, confirmed by an audit of
`internal/billing/webhook.go` before this table was added: `onInvoice` read an invoice event's
`customer`/`subscription` fields only, to attribute and reconcile the subscription, and discarded
everything else (amount, status, invoice id) after that. Owner Hub payment history required real,
normalized data - not a per-page live Stripe fetch and not fabricated numbers.

**`billing_transactions`** (migration `0038_billing_transactions`) is the new table, populated
**only** from `invoice.paid` and `invoice.payment_failed` webhook events - the two invoice events
Champion already subscribes to. One row per processed event:

| Column | Source |
|---|---|
| `organization_id` | resolved the same way every other webhook event is (`Service.resolveOrgID`) |
| `provider_invoice_id` | the invoice event's own `id` |
| `provider_payment_intent_id`, `provider_subscription_id` | the invoice event's `payment_intent`/`subscription` |
| `status` | `PAID` for `invoice.paid`, `FAILED` for `invoice.payment_failed` - **never** `OPEN`/`VOID`/`REFUNDED`: Champion does not subscribe to `invoice.voided` or a refund event, so those states are never fabricated (task Part D.17) |
| `amount_cents` | `amount_paid` (paid) or `amount_due` (failed) - never re-derived from subscription/price state |
| `currency`, `period_start`, `period_end` | the invoice event's own currency and first line item's period (Stripe invoices carry period per line, not one top-level period - `internal/billing/webhook.go`'s `webhookInvoice.period()`) |
| `paid_at` / `failed_at` | stamped at processing time, whichever this event type is |
| `stripe_event_id` | ties the row 1:1 to the already-deduped `billing_webhook_events` row |

**Idempotency**: `RecordBillingTransaction` (`internal/repository/saas_subscriptions_repository.go`)
does `INSERT ... ON CONFLICT ON CONSTRAINT uq_billing_transactions_event DO NOTHING`, keyed on
`(provider, stripe_event_id)`. This is a defensive second layer - `HandleWebhook`'s own
`billing_webhook_events` dedupe already guarantees `onInvoice` runs at most once per Stripe event -
not the primary correctness mechanism, exactly mirroring how `RecordWebhookEventOnce` itself is
described elsewhere in this document.

**Owner-facing surface** (platform-admin only, `docs/ADMIN_API.md`):

* `GET /api/admin/billing/plans` - the full catalog (public AND private plans, unlike the customer-
  facing `GET /api/saas/billing/plans`), still never a Stripe price id (task Part C.13: Champion's
  own catalog values are enough for an Owner Hub display).
* `GET /api/admin/billing/payments` - cursor-paginated, filterable by `organizationId` and `status`,
  newest first. No `plan` filter: a transaction does not record which plan it was for (see below).
  No date-range filter: cursor pagination by id is already a stable, simple ordering that a date
  range would only complicate without a demonstrated product need.
* `GET /api/admin/organizations/{organizationID}` (existing route) now additionally returns
  `recentPayments` (newest 10) - `current plan`/`subscription status`/`trial ends`/`current period
  end`/`cancelAtPeriodEnd` were already present via the existing `subscription` block before this
  phase; only the payment history itself was new.

**`plan` is deliberately not on a payment/transaction row.** The webhook carries a Stripe price id,
not a Champion plan key, and resolving one would need a second catalog lookup that could itself be
wrong if the catalog changed since that payment - showing the organization's *current* plan next to
a historical payment would misrepresent history. Left out rather than guessed, per this phase's own
instruction not to invent state.

Tests: `internal/billing/service_test.go`'s `TestWebhookInvoicePaidPersistsOneTransaction`,
`TestWebhookInvoicePaymentFailedPersistsOneTransaction`, `TestWebhookDuplicateInvoiceDeliveryDoesNotDuplicateTransaction`;
`internal/app/admin_api_billing_integration_test.go` (real routes + real PostgreSQL): plan catalog
contents, payment list pagination/filters, the organization detail extension, and platform-admin-only
authorization (a Player, a Client OWNER and a Client ADMIN are all denied).

## 28. No-card trial (Champion Customer Onboarding V2)

Flow: **Start Free Trial -> 14 days -> configure the first service -> activate a paid subscription when ready.**

**Trial policy**

* One 14-day trial per Discord account (the acting user), with no payment method: no Stripe Checkout, customer, subscription or invoice is created to start it.
* `trial_grants(user_id PRIMARY KEY, organization_id UNIQUE)` (migration 0047) is the database-enforced record. A user's second organization never gets a trial, and an organization is never
  trialed twice, even under concurrent requests (the grant's uniqueness decides the race inside the same transaction that writes the subscription row).
* An existing trial keeps its clock; an expired trial is never restarted or extended; a row with a Stripe subscription is never overwritten.
* The 0047 backfill records every owner whose organization already had a trial, so every existing trial is preserved exactly as it is and those owners' next organization gets none.
* `EnsureTrial` remains the repository foundation; `SubscriptionRepository.StartTrial` is its policy-checked, per-account form.

**Where a trial starts.** Organization creation starts the creator's trial when they have not used it (unchanged for existing website clients). A creator who already used theirs gets an
`INACTIVE` row (`plan: "NONE"`): billing required. `POST .../trial/start` is idempotent: it starts the trial when still eligible, otherwise it returns the persisted state unchanged.

**Plan selection without checkout.** `planKey` (LOW/MEDIUM/HIGH, validated against the public catalog) is stored in `subscriptions.intended_plan`, never in `subscriptions.plan`. The intended
plan grants nothing: `plan` stays `TRIAL` until a Stripe webhook sets the paid plan.

**Conversion.** Only an explicit `POST .../billing/checkout`. The plan and price are resolved from the server catalog, Stripe trial days are 0, and the webhook applies the resulting state.
Converting during the trial bills immediately, and the trial ends when the webhook applies `ACTIVE`.

**Expiry.** When `trial_ends_at` passes without a paid subscription, `trialStatus` is `EXPIRED` and `billingRequired` is true. Setting up a new service returns `402 BILLING_REQUIRED`.
Nothing is deleted, nothing is charged, and no checkout is created automatically. Existing installations, their configuration and all community data are kept.

**Installation capacity** (`POST .../installations`, the "initial setup / new service" operation):

| Subscription | Services allowed |
|---|---|
| running trial | 1 (`billing.TrialInstallations`) |
| paid (Stripe subscription, ACTIVE / PAST_DUE / Stripe-trialing legacy) | the plan's `limits.installations` in the catalog (1 when unset; the approved LOW/MEDIUM/HIGH catalog sets 1) |
| expired trial, INACTIVE, CANCELED, SUSPENDED | none: `402 BILLING_REQUIRED` |

At capacity: `409 INSTALLATION_LIMIT_REACHED`, with a message telling the customer to reconfigure the existing service or upgrade. The count and insert run in one transaction under a row lock
on the organization, so concurrent requests cannot exceed the limit. Retrying initial setup for a guild that already has an unconfigured installation still returns `409 CONFLICT`.
Nothing existing is modified or replaced. Reconfiguring a service stays on the existing installation's routes (server selection, channels), and those are not capacity-gated.

**API**

```
GET  /api/saas/organizations/{organizationID}/trial          any member; read-only
POST /api/saas/organizations/{organizationID}/trial/start    OWNER/ADMIN; body { "planKey"?: "LOW" | "MEDIUM" | "HIGH" }
```

```ts
interface TrialState {
  trialStatus: "NOT_STARTED" | "ACTIVE" | "EXPIRED" | "NOT_ELIGIBLE" | "CONVERTED";
  subscriptionStatus: string | null;   // subscriptions.status
  selectedPlan: string | null;         // intended plan while not paying; the paid plan once CONVERTED
  trialStartedAt: string | null;       // RFC 3339
  trialEndsAt: string | null;
  daysRemaining: number;               // whole days, rounded up; 0 unless ACTIVE
  billingRequired: boolean;
  paymentMethodRequired: false;        // starting the trial never needs one
  started: boolean;                    // this request started the trial
  installationLimit: number;
  installationCount: number;
}
```

Errors: `INVALID_PLAN` (400, unknown or private plan), `FORBIDDEN` (403, not OWNER/ADMIN for `start`, not a member for `GET`), `BILLING_REQUIRED` (402) and
`INSTALLATION_LIMIT_REACHED` (409) from installation creation.

**Not enforced here.** Live runtime publishing (killfeed, feeds) for an existing installation is **not** suspended when a trial expires. `internal/entitlements.Resolve` still returns the
full feature set for every plan, by that package's existing design. Expiry blocks new services and reports `billingRequired`; gating live publishing is a separate, explicit product change.
