# C.A.S.E. Phase 6 release gates — Stripe sandbox QA

**State:** DRAFT / NO LIVE PAYMENTS. This document is not authorization to deploy
or activate purchasing. It deliberately does not contain any Stripe secret.

## Account and environment isolation (hard gate)

The connected Champions Stripe account currently exposes one **live-mode**
context. Do not create trial products, customers, subscriptions, or transactions
there to simulate the test suite. A second, explicitly **test-mode or sandbox**
Stripe context must be exposed before any Stripe mutations below. Record the
context/mode after connecting; never infer test mode from product names.

Use a disposable Railway deployment/database and a separate Stripe test
customer. Never copy a live customer ID into a test Checkout. Use separate
test-mode STRIPE_SECRET_KEY, STRIPE_WEBHOOK_SECRET and CASE_*_PRICE_ID values.
Keep production `CHAMPION_CASE_BILLING_ENABLED=false`.

Test Products and recurring Prices, only after a test context is available:
- C.A.S.E. Watch: USD 499 cents per month; Product metadata
  `champion_product_kind=CASE_ADDON`,
  `champion_case_tier=CASE_WATCH`.
- C.A.S.E. Pro: USD 999 cents per month with
  `champion_case_tier=CASE_PRO` and the same product kind.
- C.A.S.E. Command: future / non-purchasable; do not create or advertise its
  checkout until its capabilities pass verification.
- Preserve the existing base LOW/MEDIUM/HIGH products and subscriptions.

Even in test mode, do not set the checkout flag before endpoint, webhook,
server-scope and test-mode key/price verification. The website must read
`purchasable` from the backend, not hardcode availability.

## Implementation already added on draft PR

- Additive migrations 0054–0058: server-scoped subscription, pending session,
  atomic webhook ledger, paid invoice coverage, immutable one-time seven-day
  trial grant, and incrementing checkout attempt after provider-confirmed
  session expiry.
- A subscription's `ACTIVE` status alone does not prove payment.
  `invoice.paid` must include the same Stripe subscription's **exact price**
  and a subscription item period. An unrelated/proration-only invoice line
  cannot grant paid-through coverage.
- A late failed invoice for a period already paid does not revoke a newer
  confirmed paid period. A new unpaid period remains fail-closed.
- Founder trial entitlement requires matching subscription, server and an
  immutable one-time grant. Checkout does **not** yet offer a founder trial.
- Expired-session recovery is available only after Stripe confirms `expired`
  and **no** attached subscription, with exact metadata and customer match,
  a compare-and-swap on pending row and a new idempotency attempt. No Stripe
  write is performed by the recovery endpoint.

## Required test-mode scenarios (not yet executed against Stripe)

1. Validate catalog product and price metadata/amounts/currency/monthly cycle;
   reject a base price or mismatched/archived Product.
2. With billing disabled, catalog must list packages as non-purchasable;
   direct checkout must return not available and never call Stripe.
3. An owner/admin with a paid base plan may buy Watch or verified Pro for
   one bound installation. A member, other organization or other server may
   not buy or view another installation's add-on.
4. Repeated Checkout requests under race use the same session/idempotency
   attempt, never two paid Stripe subscriptions.
5. Reordered Checkout, subscription, invoice.paid and invoice.failed events:
   idempotent, correct attribution, no base LOW/MEDIUM/HIGH plan mutation.
6. An ACTIVE subscription without a verified paid invoice does not unlock
   premium workers/API. A paid invoice for a different price or subscription
   does not grant this server's access. Delayed payment and 0-amount invoices
   require an explicit, documented policy before release.
7. A Stripe-confirmed expired/unsubscribed Checkout can be recovered once
   and retried with a new idempotency key. OPEN, COMPLETE, attached-subscription,
   cross-server and forged metadata cases are all rejected.
8. Scheduled cancellation preserves confirmed coverage until the paid end;
   reactivation of the exact add-on works without changing the base plan.
9. Founder trial: verify eligibility of an existing paid customer, exact
   one-time 7-day grant per organization/game server, repeat/abandoned trial
   cases, end-of-trial charge disclosure and cancellation. Trial checkout
   remains unimplemented and must not be marketed yet.
10. Upgrade/downgrade: show provider-calculated price/proration preview
    before confirmation; downgrade default at renewal. **Not implemented.**
11. Refund, disputed invoice, subscription deletion, customer portal
    cancellation, missing webhook, and reconciliation recovery. **Not signed
    off.**
12. Premium API/Go worker/Discord access must use the same server-scoped
    resolver. Existing free observational data and evidence must remain intact.
    No automatic bans, guaranteed device detection, or unverified detectors.

## Release checklist

- [ ] Connected Stripe sandbox context explicitly reports `livemode=false`.
- [ ] Separate test Products, Prices, keys, webhook endpoint, test customer.
- [ ] Disposable test DB and deployment; no live Nitrado writes/restarts.
- [ ] Backend and website current draft heads pass CI.
- [ ] Approved purchase copy and accurate feature inventory.
- [ ] Trial eligibility, upgrades/downgrades, disputes and canceled Checkout
      lifecycle implemented and signed off.
- [ ] Full Stripe sandbox checkout + webhook replay evidence captured.
- [ ] Premium enforcement at every relevant API and background worker.
- [ ] Operator approves the exact production prices, webhook destination,
      rollout tier, flag and deployment; only then enable live checkout.

**Failure policy:** keep checkout disabled, preserve customer records/evidence,
and do not substitute live Stripe for missing test-mode access.
