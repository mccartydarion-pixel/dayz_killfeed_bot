# C.A.S.E. Phase 6 — billing and entitlement implementation contract

Status: foundation / implementation not enabled. This document is the execution contract; it does not itself activate or sell access.

## Existing system and non-negotiable invariants

- Existing base subscriptions are stored in `subscriptions` with a unique `organization_id`. Do **not** reuse that row, `subscriptions.plan`, `provider_subscription_id`, or the existing base plan catalog as the paid C.A.S.E. product record.
- Reuse the existing Stripe customer and one Champions login/dashboard/billing surface. Stripe's customer can own multiple subscription objects; a consolidated billing view is not a guarantee of a single invoice or renewal date. Do not advertise a single invoice unless billing-cycle alignment and actual Stripe invoice behavior are tested.
- Current base Stripe subscription normalization assumes `Items.Data[0]` is the base price; current webhook dispatcher applies all subscription events to base access. Any C.A.S.E. subscription routed through that logic can overwrite the base plan. Add explicit product-kind dispatch **before** existing base reconciliation, with add-on webhook handling isolated from existing rows.
- Current core `entitlements.Resolve(plan)` grants all base keys: do not infer C.A.S.E. from that helper or add anti-cheat keys to its unconditional list. Create a server-level security entitlement lookup, enforced by Go API and processing workers.
- Preserve the existing no-card 14-day base trial. Seven-day C.A.S.E. Pro founder trial is a separate, one-time, server-scoped offer and must not reset or extend the base plan trial.
- The current C.A.S.E. observation/evidence endpoints report detectors disabled and enforcement disabled. Only advertise/enable existing verified capabilities. No automated bans, alleged device identification, or unverified detectors.

## Product catalog (approved proposed list prices, USD, monthly, each server)

| Key | Price cents | Availability |
| --- | ---: | --- |
| CASE_WATCH | 499 | Launch after capability verification |
| CASE_PRO | 999 | Launch after capability verification |
| CASE_COMMAND | 1499 | Future; non-purchasable until verified |

Create separate Stripe Product/Price pairs in **test mode first**. Never overwrite existing Low/Medium/High Product/Price objects. Price IDs are server-side configuration, not client authority. No live-mode Stripe mutation or charge without the release gate.

## Model and lifecycle

Add an additive migration for `case_addon_subscriptions`, with fields: id, organization_id, installation_id (or authoritative linked server ID with FK), tier, status, provider, provider_customer_id, provider_subscription_id, provider_price_id, current_period_start/end, trial_start/end, cancel_at_period_end, created_at, updated_at. Use unique constraints on (organization_id, installation_id) for a single current entitlement and on non-null provider_subscription_id. If historical rows are required, separate immutable history from current state. Validate that selected server belongs to organization and user has owner/admin billing capability, with cross-guild isolation.

Keep an immutable subscription-event log/idempotency record separate from the base handler or extend its existing unique-event transaction. A replayed event must be safe, and a failed processing attempt must not be permanently acknowledged/deduped before successful reconciliation. Classify both Checkout Sessions and subscription/invoice events using server-authored metadata plus persisted Stripe subscription ID, never by trusting a browser-supplied amount, tier, organization, or server ID. Unknown/invalid kind must not be allowed to overwrite base state. Reconcile from Stripe current state for out-of-order updates; use transactionally persisted subscription state.

Prefer a distinct Stripe subscription for each server-scoped add-on on the same Stripe customer: it preserves one tier per server and avoids the existing first-item-only base normalizer. If alignment to one invoice is required later, explicitly design and verify that with Stripe before advertising. A second server on the same customer must never inherit the first server's entitlement.

The add-on checkout request is `{installationId, tier}`; its price and customer resolve server-side. Prevent concurrent duplicate checkouts with an idempotency key per organization/server/attempt, and re-check active add-on state. Verify checkout completion through Stripe webhooks/reconciliation, not redirects. Upgrade/proration preview and confirmation precede mutation. Downgrade at period end unless explicitly shown otherwise. Cancellation at period end retains access through the paid interval. Payment failure must apply a deliberate grace policy, not automatically remove existing base access.

## Server entitlement contract

Provide `case.watch`, `case.pro`, `case.command` as server-scoped capabilities; tiers are cumulative where features actually exist. Gate UI and Go API; paid workers must also gate resource-intensive premium processing. Base subscription remains a prerequisite for eligible paid C.A.S.E. access; define grace behavior explicitly. Fail closed for paid access if entitlement cannot be confirmed, without deleting stored evidence. An organization's admin may view purchases only for their owned servers. Discord integration reads the same authoritative state.

## API and website handoff

The implemented endpoints on the existing Go service are:
- `GET /api/saas/billing/case/plans` — backend-derived catalog and purchasable flags.
- `GET /api/saas/organizations/{organizationID}/billing/case/servers` — scoped purchases, period, verified paid coverage and cancellation.
- `POST /api/saas/organizations/{organizationID}/billing/case/checkout` — owner/admin, selected installation and tier, server-validated price.
- `POST /api/saas/organizations/{organizationID}/billing/case/cancel` and `/reactivate` — owner/admin; scoped to one selected installation. Cancellation works even with new sales disabled.
- No plan-change endpoint yet. Do not advertise upgrade/downgrade or trial as available.

The website implementation is a separate draft PR in `Champions_Killfed_Website`; it extends the existing Subscription page. Stripe IDs are never sent to the browser.

## Work sequence / release gates

1. Add the isolated schema, model, catalog validator, and read-only status/query tests.
2. Add Stripe test-mode products and prices; record their IDs in environment-specific server config.
3. Add isolated provider methods, signed webhook dispatch and reconciliation, event replay tests, attribution tests and cross-server isolation tests.
4. Add Go API and server authorization, then site controls. Initially disable checkout behind `CHAMPION_CASE_BILLING_ENABLED=false`.
5. Add trial eligibility ledger keyed to org/server (one grant per server), cancellation and upgrade/downgrade tests, payment failure handling, and complete audit logging.
6. Exercise checkout, duplicate callbacks, out-of-order events, failed payment, plan changes, cancellation, subscription recovery and C.A.S.E. disabled/not-yet-verified cases in Stripe test mode.
7. Release only after production telemetry capability verification, approved public copy, explicit operator check of Stripe live products/prices/webhook destination and feature flag.

Regression invariant: all existing LOW/MEDIUM/HIGH subscriptions, no-card base trials, plan changes, webhooks, invoices, auth, and base entitlements behave exactly as before when the C.A.S.E. flag is off.

## Phase 6.2 implementation status (draft / disabled)

Added migration 0055 for a single pending checkout reservation and transactionally
recorded C.A.S.E. webhook delivery. The Go API exposes read-only catalog/status
and owner/admin checkout; `CHAMPION_CASE_BILLING_ENABLED` is false by default.
Both the Checkout Session and Stripe subscription receive server-authored
product-kind, organization, installation, game-server, add-on and tier metadata.
Distinct add-on subscriptions use the existing Stripe customer. Webhook dispatch
classifies C.A.S.E. **before** base subscription reconciliation, with a separate
transactional deduplication ledger. Configured Stripe Price IDs must match
approved recurring amounts and explicitly tagged Stripe Products. An unknown
price or mismatched ownership fails closed.

Phase 6.3 adds a paid-coverage migration (0056). Stripe `ACTIVE` and
`checkout.session.completed` alone DO NOT grant paid access. Only a signed
`invoice.paid` event containing a subscription billing-period end advances
`paid_through` transactionally; subsequent subscription updates cannot erase
it. An ACTIVE add-on entitlement requires current `paid_through` and verified
rollout tier. A TRIAL add-on instead requires a genuine unexpired Stripe trial.
The exact linked game server and active paid base remain mandatory. The
`/cancel` and `/reactivate` routes update only a matching C.A.S.E.
subscription after fetching and validating Stripe metadata. They do not change
base billing, and a scheduled cancellation retains confirmed paid access.

Unresolved release gates: reconcile canceled/expired or abandoned Checkout
Sessions safely; create and verify test-mode Stripe Products/Prices; design a
one-time founder trial eligibility ledger and previewed upgrades/downgrades;
confirm actual C.A.S.E. feature availability; implement refund/payment dispute
handling and full Stripe lifecycle QA. No production deployment or live Stripe
catalog mutation is part of this draft.

## Phase 6.4 release-gate hardening (draft / no sales)

- Fixes the scoped subscription listing to read its `paid_through` column;
  the per-server subscription page must not fail once add-on rows exist.
- Adds an immutable one-time `case_addon_trial_grants` ledger keyed by
  organization and game server, with a DB-enforced maximum seven-day Pro grant.
  Trial entitlements now require this exact persisted grant. A generic Stripe
  `trialing` status cannot activate C.A.S.E. Pro. The trial checkout offer
  remains disabled until sandbox payment/eligibility verification.
- Adds monotonically increasing `checkout_attempt` and
  `POST .../billing/case/checkout/recover` for an OWNER/ADMIN. This route
  only reads the exact Stripe Checkout Session and clears the pending local
  reservation if Stripe says **expired**, has **no subscription**, and its
  customer and all server-bound metadata match. It creates no payment.
  A new purchase uses a new Stripe idempotency key. Late events from the
  expired session cannot bind to the new attempt.
- Requires `invoice.paid` to contain a paid matching-price **subscription
  line** with the exact subscription ID. Another invoice item cannot extend
  C.A.S.E. coverage. A stale failed invoice cannot revoke a subsequently
  confirmed paid period; a failure for a genuinely newer period still fails
  closed. Existing LOW/MEDIUM/HIGH invoice logic remains unchanged.
- Sandbox setup and the full payment/release matrix are documented in
  `docs/CASE_STRIPE_TESTMODE_QA.md`. The connected Stripe context is
  live-mode only. No Stripe test-mode transactions or live changes were made.

The website draft now includes an OWNER/ADMIN expired-session check and
same-tier retry only after the backend reports a safe pending reset.

Remaining: test-mode Stripe products/prices and webhook simulations, explicit
founder eligibility and checkout disclosure, proration
preview and upgrade/downgrade, disputes/refunds, and real feature-level
entitlement enforcement across Go APIs/workers. No production deployment or
live billing activation is authorized by this draft.

## Phase 6.6 — server-enforced premium access (draft)

- Sales `CHAMPION_CASE_BILLING_ENABLED` and access
  `CHAMPION_CASE_ACCESS_ENABLED` are **separate**, default-off flags.
  Starting sales while access is off is a startup error. Sales may be turned
  off without revoking an existing paid subscription's separately enabled
  access. Both remain false in production.
- The shared Go `billing.Service.CaseAccess/CaseAllows` resolver reloads
  authoritative base and add-on database rows for each request/job. A premium
  capability requires an unexpired ACTIVE paid Stripe base subscription
  (with real customer, subscription and price IDs), a C.A.S.E. subscription
  with the **same Stripe customer**, the exact selected installation and
  bound game server, verified invoice-paid coverage or immutable founder
  trial grant, the backend's exact tier-to-Stripe-price mapping (retained even
  when sales are paused), plus the independently verified rollout tier. Missing database
  access fails closed; no website, Discord role or return URL can grant access.
- `GET .../admin/anti-cheat/entitlements` is a staff-authorized **read-only
  presentation snapshot**; it is not proof for downstream authorization.
  `GET .../admin/anti-cheat/premium/evidence-export` requires both
  `CapPlayerLocationView` and server-side `case.pro`, runs bounded
  100-row repository reads (max 250 rows/request), and preserves source
  provenance with scoped `guild_id` and `server_id` on each query. This
  export never estimates movement, detects devices, labels a cheater or bans.
  Existing observation/evidence/session/source-integrity endpoints remain
  unchanged and accessible under their existing staff permissions.
- `App.caseWorkerAllowed` invokes the same resolver. **No premium background
  worker or Discord premium publisher is enabled yet**. When introduced,
  each paid operation must call this gate before processing/publishing and
  fail closed on a store error. The current allowlisted observational evidence
  collector remains independent of a paid entitlement to preserve existing
  opt-in evidence, including on subscription expiry. Historical evidence
  is never deleted when paid access ends.
- Website shows only server-reported premium entitlement status, displays
  unavailability instead of guessing when the read fails, and does not
  substitute demo evidence for a premium API error.

Unresolved: full new premium worker/Discord feature integration, live
capability verification, test-mode Stripe checkout/webhook QA, founder
trial Checkout eligibility, plan change/refund/dispute flows and operator
approval before production rollout. The gated export is a draft API, not
authorization to market or sell Watch/Pro.
