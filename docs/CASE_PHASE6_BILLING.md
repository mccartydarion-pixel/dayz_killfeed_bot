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

- `GET /api/saas/case/billing/catalog`: verified purchasable packages and public pricing.
- `GET /api/saas/case/billing/servers`: linked servers and tier/status/renewal/cancellation, scoped to organization.
- `POST /api/saas/case/billing/checkout`: owner/admin, validated installation, server-owned tier and price.
- `POST /api/saas/case/billing/change`: scoped existing add-on, preview before confirmation.
- `POST /api/saas/case/billing/cancel` and `/reactivate`: scope + Stripe reconciliation.
- Extend the existing Subscription page with Security Add-ons, linked-server selection, verified status, price, renewal details, optional founder trial, and customer portal access. Do not hardcode Stripe price IDs in the website.

## Work sequence / release gates

1. Add the isolated schema, model, catalog validator, and read-only status/query tests.
2. Add Stripe test-mode products and prices; record their IDs in environment-specific server config.
3. Add isolated provider methods, signed webhook dispatch and reconciliation, event replay tests, attribution tests and cross-server isolation tests.
4. Add Go API and server authorization, then site controls. Initially disable checkout behind `CHAMPION_CASE_BILLING_ENABLED=false`.
5. Add trial eligibility ledger keyed to org/server (one grant per server), cancellation and upgrade/downgrade tests, payment failure handling, and complete audit logging.
6. Exercise checkout, duplicate callbacks, out-of-order events, failed payment, plan changes, cancellation, subscription recovery and C.A.S.E. disabled/not-yet-verified cases in Stripe test mode.
7. Release only after production telemetry capability verification, approved public copy, explicit operator check of Stripe live products/prices/webhook destination and feature flag.

Regression invariant: all existing LOW/MEDIUM/HIGH subscriptions, no-card base trials, plan changes, webhooks, invoices, auth, and base entitlements behave exactly as before when the C.A.S.E. flag is off.
