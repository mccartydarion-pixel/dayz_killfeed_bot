# C.A.S.E. Phase 6 release gates — Stripe sandbox QA

**State:** DRAFT / NO LIVE PAYMENTS. This document is not authorization to deploy
or activate purchasing. It deliberately does not contain any Stripe secret.

## Account and environment isolation (hard gate)

The Stripe ChatGPT plugin connection is currently unavailable after the
sandbox OAuth callback failure. The earlier connection exposed only a
**live-mode** context, not a usable sandbox. Do not create trial products, customers, subscriptions, or transactions
there to simulate the test suite. A second, explicitly **test-mode or sandbox**
Stripe context must be exposed before any Stripe mutations below. Record the
context/mode after connecting; never infer test mode from product names.

Use a disposable Railway deployment/database and a separate Stripe test
customer. Never copy a live customer ID into a test Checkout. Use separate
test-mode STRIPE_SECRET_KEY, STRIPE_WEBHOOK_SECRET and CASE_*_PRICE_ID values.
Keep production `CHAMPION_CASE_BILLING_ENABLED=false` and
`CHAMPION_CASE_ACCESS_ENABLED=false`.

Test Products and recurring Prices, only after a test context is available:
- C.A.S.E. Watch: USD 499 cents per month; Product metadata
  `champion_product_kind=CASE_ADDON`,
  `champion_case_tier=CASE_WATCH`.
- C.A.S.E. Pro: USD 999 cents per month with
  `champion_case_tier=CASE_PRO` and the same product kind.
- C.A.S.E. Command: future / non-purchasable; do not create or advertise its
  checkout until its capabilities pass verification.
- Preserve the existing base LOW/MEDIUM/HIGH products and subscriptions.

## Operator-supplied candidate Price IDs (unverified, 2026-09-25)

The owner supplied the following public IDs as intended **test-mode**
C.A.S.E. catalog candidates. Receipt of IDs is not independent proof of
Stripe mode, amount, interval, product linkage, metadata or active status.
Do not set these in production or turn on checkout yet.

| Tier | Intended product | Intended Price | Expected |
| --- | --- | --- | --- |
| Watch | `prod_VKAAcsjmLTvLGs` | `price_1UJVqQ9sqOgctIAtK8UiKgJP` | USD 499 cents/month |
| Pro | `prod_VKABdveP3lpSWd` | `price_1UJVqp9sqOgctIAtFWedx67E` | USD 999 cents/month |

Only after a **read-only Stripe sandbox API check** confirms both Price
and Product `livemode=false`, both active, exact parent product IDs,
expected USD/monthly recurring amounts and Product metadata
(`champion_product_kind=CASE_ADDON`, tier-specific
`champion_case_tier`), set these on an **isolated staging service**:

```text
CHAMPION_CASE_WATCH_PRICE_ID=price_1UJVqQ9sqOgctIAtK8UiKgJP
CHAMPION_CASE_PRO_PRICE_ID=price_1UJVqp9sqOgctIAtFWedx67E
CHAMPION_CASE_COMMAND_PRICE_ID=
CHAMPION_CASE_BILLING_ENABLED=false
CHAMPION_CASE_ACCESS_ENABLED=false
```

Do not commit a secret key, alter the existing base plan Price mappings,
or enable any production feature flag. The per-server test checkout
requires a separate test customer, test webhook secret, disposable DB and
explicit operator approval after validation.

Even in test mode, do not set the checkout flag before endpoint, webhook,
server-scope and test-mode key/price verification. The website must read
`purchasable` from the backend, not hardcode availability.

## Implementation already added on draft PR

- Additive migrations 0056–0060: server-scoped subscription, pending session,
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

## Isolated Railway staging (prepared, not deployed)

A separate, private Railway project **champions-case-staging** now exists.
Its empty service **case-billing-qa** has no GitHub source, no database and
no runtime deployment. Railway named this new project's default environment
`production` automatically; it is NOT the live Champions project's
production environment. Its service has only these nonsecret variables:
`APP_ENV=staging`, the two candidate test Price IDs above, blank Command
Price ID, `CHAMPION_CASE_BILLING_ENABLED=false`, and
`CHAMPION_CASE_ACCESS_ENABLED=false`. The variables were set without
triggering a deployment. Do not copy the existing Champions production
environment or its Stripe, Discord, Nitrado or database secrets into it.

### Read-only sandbox object verification

Pull the draft branch locally and run the following from the backend repo
using an independent **test/sandbox** API key. The verifier makes exactly
two Stripe Price GET requests with expanded Products. It checks the real
Price and Product `livemode=false`, IDs, active flags, fixed USD monthly
amounts and exact metadata. It performs no Checkout or other Stripe write.

```powershell
git pull --ff-only origin feat/case-billing-phase6-foundation
$secure = Read-Host "Enter STRIPE SANDBOX secret or restricted test key" -AsSecureString
$ptr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
try {
    $env:CASE_STRIPE_TEST_SECRET_KEY = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($ptr)
} finally {
    [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($ptr)
}
try {
    go run ./cmd/case-stripe-preflight
} finally {
    Remove-Item Env:\CASE_STRIPE_TEST_SECRET_KEY -ErrorAction SilentlyContinue
}
```

Only `sk_test_` / `rk_test_` keys are accepted; never substitute the
live `STRIPE_SECRET_KEY`. Record only the PASS/failure lines, not the key.
After both objects pass the real API check, next configure an isolated
disposable staging DB, test customer, test-mode webhook and staging service
source. Then exercise no-sale/read-only behavior before separately approving
test checkout. Do not deploy the current draft or enable a sales flag merely
because these prices validate.

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
    resolver. Exercise queued Watch summary followed by payment failure,
    selected server change, role revocation, missing/changed staff route and
    database error **before dequeue**; no paid message may send. The explicit
    action must reject clients without access, throttle a server, and not
    claim delivery merely because a queue accepted the message. Check
    collector-disabled/historical counts do not imply absence of events.
13. Pro export: each page must reauthorize, preserve exact org/install/server,
    obey the 250-row backend bound and 100-row website page limit, keep raw
    Nitrado paths out, and quote/neutralize spreadsheet formulas in CSV.
14. Existing free observations, evidence, sessions and ADM worker must remain
    intact when C.A.S.E. expires or is absent. No automatic bans, guaranteed
    device detection or unverified detectors.

## Release checklist

- [ ] Connected Stripe sandbox context explicitly reports `livemode=false`.
- [ ] Separate test Products, Prices, keys, webhook endpoint, test customer.
- [ ] Disposable test DB and deployment; no live Nitrado writes/restarts.
- [ ] Backend and website current draft heads pass CI.
- [ ] Approved purchase copy and accurate feature inventory.
- [ ] Trial eligibility, upgrades/downgrades, disputes and canceled Checkout
      lifecycle implemented and signed off.
- [ ] Full Stripe sandbox checkout + webhook replay evidence captured.
- [ ] Verify the implemented durable outbox and private staff-channel gate
      against real Discord + multiple staging replicas. PostgreSQL admission,
      claim fencing, returned-message receipts, original-actor reauthorization
      and pre-send retry/UNKNOWN logic have disposable-DB tests. An ambiguous
      network send must never be automatically repeated.
- [ ] Review actual Discord staff roles and the manually selected channel;
      permission checks cannot infer all organizational role assignments.
- [ ] Write and exercise operator reconciliation for UNKNOWN / a lost receipt,
      including confirming whether Discord actually holds the message.
- [ ] Premium enforcement at every newly introduced paid API/worker and
      proof that free observational features remain available.
- [ ] Operator approves the exact production prices, webhook destination,
      rollout tier, flag and deployment; only then enable live checkout.

**Failure policy:** keep checkout disabled, preserve customer records/evidence,
and do not substitute live Stripe for missing test-mode access.
