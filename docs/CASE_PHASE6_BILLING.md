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
- `POST .../billing/case/plan/preview` then `POST .../billing/case/plan` — owner/admin; Watch <-> Pro on the server's existing add-on subscription (Phase 6.10). Founder trial is still not offered.

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

Added migration 0057 for a single pending checkout reservation and transactionally
recorded C.A.S.E. webhook delivery. The Go API exposes read-only catalog/status
and owner/admin checkout; `CHAMPION_CASE_BILLING_ENABLED` is false by default.
Both the Checkout Session and Stripe subscription receive server-authored
product-kind, organization, installation, game-server, add-on and tier metadata.
Distinct add-on subscriptions use the existing Stripe customer. Webhook dispatch
classifies C.A.S.E. **before** base subscription reconciliation, with a separate
transactional deduplication ledger. Configured Stripe Price IDs must match
approved recurring amounts and explicitly tagged Stripe Products. An unknown
price or mismatched ownership fails closed.

Phase 6.3 adds a paid-coverage migration (0058). Stripe `ACTIVE` and
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
- `App.caseWorkerAllowed` invokes the same resolver. A paid Watch staff
  message is submitted to the existing bounded Discord ADMIN_ALERTS worker
  only after an explicit authorized request. The worker rechecks the same
  paid capability **after route resolution and immediately before send**;
  canceled/repointed/expired/missing access never uses stale queued state.
  No automatic premium processing or scheduled publisher has been enabled. The current allowlisted observational evidence
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

## Phase 6.7 — first gated Watch delivery and Pro export UI (draft)

- `POST .../admin/anti-cheat/premium/watch-digest` requires location-view
  staff permission, server-scoped `case.watch`, available PostgreSQL, an
  explicit ADMIN_ALERTS route and a manual request. It counts already
  persisted ADM evidence lines, hits and kills in a 24-hour **ingestion**
  window, without player identities, guessing shot counts or a second Nitrado
  poll. `202 queued` does **not** claim Discord delivery.
- The pre-existing, bounded AdminAlertPublisher queue now contains a
  C.A.S.E.-specific paid envelope. It rejects mismatched/blank identifiers,
  a generic free alert carrying a paid scope, an unscoped digest, missing
  authorizer and unpaid/errored/repointed access at dispatch. Free ADM/zone
  operational alerts continue unchanged. The embed is informational,
  labeled observation-only, and contains no cheat verdict or auto-action.
- HTTP also checks the ADMIN_ALERTS route before enqueueing. A process-local
  per-server 1/hour cooldown provides initial flood protection. **Cross-
  replica durable deduplication, delivery acknowledgement/retry and
  verification that manually selected staff routes are appropriately private
  are outstanding release gates**. The selected admin channel may be
  customer-configured; the website explicitly reminds operators to restrict
  its visibility before sending.
- The website provides an explicit Watch button only when the server's
  read-only entitlement snapshot reports `case.watch`, while the actual
  server action/queue worker independently enforce access again.
- Pro now has a server-side-authorized, bounded evidence page in the live
  workspace and a CSV download of **that page only** (100 rows requested;
  backend hard cap 250). It includes hashed source reference, original ADM
  clock, ingestion timestamp and byte offset without raw Nitrado source
  paths. CSV quotes cells and neutralizes formula prefixes in player-
  controlled strings. Paging always makes a fresh authorized backend read.
  No demo evidence is used as a fallback.

Both checkout and premium access still default OFF. These are draft
capabilities, not authorization to merge, activate, advertise as released,
or use live Stripe. Complete the sandbox and QA checklist before launch.

## Phase 6.8 — durable Watch delivery and private staff route (draft)

This section **supersedes the in-memory Watch queue and process-only cooldown**
described in Phase 6.7. Ordinary free ADMIN_ALERTS messages still use their
existing queue. Paid Watch requests use only the dedicated database outbox;
the original in-memory publisher has no paid authorizer in the application.

- Migrations 0061–0062 add PostgreSQL outbox and requesting actor. New
  requests are locked and admitted under the installation row with a
  rolling one-hour server cooldown. Exact org/installation/guild/server
  identity is validated by the DB; competing replicas cannot insert two
  staff digests by bypassing a local limiter.
- The dedicated worker, started only if premium access is enabled, uses
  SKIP LOCKED + a claim version and a two-minute pre-send lease. Only
  READY and expired CLAIMED messages retry, up to three pre-send attempts.
  The worker rechecks original requester permission (fresh Discord REST for
  nonowners), current paid access, exact server binding and direct DB route.
- The channel check fetches current Discord guild/channel/category data,
  rejects public @everyone viewing and broad role/member view allows, and
  requires the bot's View/Send/Embed permissions. This is a conservative
  gate, **not** a complete audit of every operator role: Discord admins
  can bypass permission overwrites, and roles still require human review.
  A route change is checked again atomically when entering SENDING.
- SENDING is persisted BEFORE the Discord network call. Discord's returned
  message ID and channel form the SENT receipt. A timeout, lost acknowledgement
  or crash after starting a send becomes UNKNOWN, which is **never blindly
  resent**; an operator must reconcile against the Discord channel. Thus the
  worker provides at-most-once automatic sends, not exactly-once external
  delivery. A pre-send revocation/privacy failure is BLOCKED, while transient
  pre-send lookup failures can retry with backoff. Orphaned SENDING rows are
  exposed as UNKNOWN by the sweep.
- A scoped staff receipt endpoint reports READY/CLAIMED/SENDING/SENT/UNKNOWN/
  BLOCKED, and the website offers an explicit receipt check. 202 means
  persisted, not delivered. The receipt is hidden after server repointing.
  The source-observation collector and free alert routes are unaffected.
- Disposable PostgreSQL tests cover duplicate admission across replicas,
  two-stage lease/claim fencing, simulated Discord acknowledgement loss,
  payment/role/privacy/route revocation, receipt isolation and recovery
  without replaying an uncertain external message.

Pending release gates: end-to-end Discord networking, operator role/channel
privacy review, reconciliation runbook for UNKNOWN, Stripe sandbox lifecycle,
founder trial checkout eligibility, plan changes and dispute/refund handling.
Both `CHAMPION_CASE_BILLING_ENABLED` and `CHAMPION_CASE_ACCESS_ENABLED`
remain false in production. This is a draft, not approval to merge or deploy.

## Phase 6.9 — exact Discord message reconciliation (draft)

The Phase 6.8 UNKNOWN state is a deliberate no-replay boundary. Every
dedicated paid Watch digest now includes a visible immutable
`CHAMPION-CASE-WATCH-<deliveryId>` reference field. This is a public
correlation string, not a credential, payment receipt or authorization token.

The new owner-only
`POST .../admin/anti-cheat/premium/watch-digest/{deliveryID}/reconcile`
accepts only an exact Discord message snowflake. It reads the recorded
original channel and UNKNOWN receipt from the authenticated organization and
installation, fetches the exact message from Discord, and requires the
current bot identity, saved channel, Watch observation title and precise
delivery reference. Only then does a scoped database compare-and-swap
transition UNKNOWN to SENT, retain the externally verified message ID and
audit the action. The route cannot requeue, send, create a paid entitlement
or rewrite an already SENT/BLOCKED receipt. A changed selected server cannot
reconcile a former server's receipt.

The website exposes this form only when the server's own admin-me level is
OWNER and the receipt reports UNKNOWN. All responses continue to come from
the backend. Local tests exercise discordgo send/read JSON via an isolated
`httptest` HTTP server; they never call real Discord. PostgreSQL integration
tests cover forged authors, channel/reference mismatch, tenant isolation,
double reconciliation and no resend after verification. See
`docs/CASE_DISCORD_DELIVERY_RECONCILIATION.md` for an operator runbook.

**Not a launch sign-off:** a real dedicated QA guild, manually reviewed
channel/roles, bot credential in staging (not pasted into chat), and a
controlled lost-ack test are still required. Neither production paid access
nor checkout is enabled or deployed.

## Migration numbering (renumbered 2026-09-25)

The Phase 6 migrations were renumbered, before any deployment, to follow the Shop delivery ledger. The Shop owns 0054 (`0054_shop_delivery_attempts`, PR #97) and 0055 (`0055_shop_delivery_attempt_evidence`, PR #98). Production has applied only up to `0053_installation_embed_activation`; this was verified read-only on 2026-09-25.

* SQL is byte-identical, and the relative order is unchanged.
* Earlier notes that cite 0054–0060 refer to these migrations under their new numbers.

| Old | New |
|---|---|
| 0054 | `0056_case_addon_subscriptions` |
| 0055 | `0057_case_checkout_reconciliation` |
| 0056 | `0058_case_payment_confirmation` |
| 0057 | `0059_case_founder_trial_ledger` |
| 0058 | `0060_case_checkout_attempt` |
| 0059 | `0061_case_watch_digest_outbox` |
| 0060 | `0062_case_watch_requester` |

`TestMigrationRegistryNumbersAreUniqueAndOrdered` now fails CI on any future number collision.

## Phase 6.10 — Watch/Pro plan changes and re-subscription (draft)

**Mutual exclusivity.** A server has at most one *current* add-on row, and that row's Stripe
subscription has exactly one price item. A tier change swaps that single price (`caseSingleItem`
refuses anything else), so Watch and Pro can never bill side by side and no second subscription
is created.

**Tier authority.** The current tier is derived from the Stripe price via the server-side mapping
(`CHAMPION_CASE_*_PRICE_ID`). `champion_case_tier` metadata now means "originally purchased tier"
and is not rewritten. Binding metadata (add-on, organization, installation, game server, customer)
is still verified on every event and before every Stripe write. An unconfigured price fails
closed. The repository accepts a tier change only on the row already bound to that same Stripe
subscription.

**Entitlement follows `paid_tier`** (migration `0063_case_plan_changes`). A signed `invoice.paid`
records the tier of its own qualifying line: a positive-amount `subscription_item_details` line for
this subscription with a configured C.A.S.E. price. A later period end wins; on the same period end
the higher tier wins; an older period never changes it. Access is the `paid_tier` capability set
while `paid_through` is current, still bounded by rollout verification, the paid base plan, the
exact server and the same customer.

| Change | Stripe call | Charge | Access |
| --- | --- | --- | --- |
| Upgrade Watch→Pro | price swap, `proration_behavior=always_invoice`, `payment_behavior=pending_if_incomplete`, `proration_date` from the preview | Stripe-calculated proration now | Pro only after that proration `invoice.paid`. A declined payment leaves Stripe on Watch (`PAYMENT_PENDING`); nothing local changes |
| Downgrade Pro→Watch | price swap, `proration_behavior=none` | none now; Watch price at renewal | Paid Pro kept until `paid_through`; the renewal invoice moves access to Watch |
| Restore (back to the already-paid tier inside the period) | price swap, `proration_behavior=none` | none | unchanged (already paid) |

No Subscription Schedule is used, so scheduled cancellation keeps working on a plain subscription.
Rules: ACTIVE add-ons only (not TRIAL/PAST_DUE/PENDING/CANCELED); a scheduled cancellation must be
reactivated first; one pending upgrade at a time; upgrades require the sales flag and the active
paid base on the same customer; downgrades/restores work with sales paused. A preview's
`prorationDate` must be confirmed within 15 minutes. The Stripe idempotency key is
`champion-case-tier-{addon}-{fromPrice}-{toPrice}-{prorationDate}`.

**Payment-failure policy (now explicit).** Base LOW/MEDIUM/HIGH access is never touched. A failed
renewal ends C.A.S.E. access at the end of the paid period (`PAST_DUE`, no grace entitlement). A
failed invoice for an already-paid period (e.g. a declined upgrade proration) is stale and keeps
ACTIVE. A later `invoice.paid` restores access.

**Re-subscription.** `uq_case_addon_org_installation` / `uq_case_addon_org_server` became partial
unique indexes over `status <> 'CANCELED'`. A canceled add-on is kept as history (its Stripe
subscription id stays unique). A new checkout creates a new row and a new idempotency identity, so
late events for the old subscription only ever reach the old row. The founder-trial ledger is keyed
by organization + game server and still cannot be granted twice.

**API.**

```ts
// POST .../billing/case/plan/preview  (OWNER/ADMIN; read-only)
{ installationId: number; tier: "CASE_WATCH" | "CASE_PRO" }
-> { kind: "UPGRADE" | "DOWNGRADE" | "RESTORE"; currentTier; targetTier; amountDueNowCents; currency;
     prorationDate: number; effectiveAt: string | null; nextRenewalAmountCents; currentPeriodEnd }
// POST .../billing/case/plan  (OWNER/ADMIN)
{ installationId; tier; prorationDate }   // prorationDate echoed from the preview, never an amount
-> { kind; status: "APPLIED" | "PAYMENT_PENDING"; tier; paidTier; paidThrough; currentPeriodEnd }
```

`GET .../billing/case/servers` adds `paidTier` and `pendingDowngrade`. Errors: `CASE_PREVIEW_EXPIRED`
(409); `CASE_CHECKOUT_CONFLICT` (409: unchanged tier, cancellation scheduled, change pending, not
manageable, needs reconciliation); `CASE_BASE_REQUIRED`; `CASE_NOT_AVAILABLE`; `INVALID_PLAN`.

Tests: `internal/billing/case_plan_change_test.go`, `internal/casebilling/access_test.go`
(`TestAccessFollowsPaidTier`), `internal/repository/case_plan_change_integration_test.go`,
`internal/app/saas_api_case_plan_change_integration_test.go`,
`internal/database/migrations_case_addon_test.go`. All use FakeProvider. **Nothing in this phase has
been executed against Stripe yet** - see `CASE_STRIPE_TESTMODE_QA.md` "Sandbox lifecycle run".
