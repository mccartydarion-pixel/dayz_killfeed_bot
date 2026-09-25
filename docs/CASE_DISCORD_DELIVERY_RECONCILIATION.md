# C.A.S.E. Watch — Discord delivery QA and UNKNOWN reconciliation

**Status: draft / staging-only.** Neither C.A.S.E. sales nor premium access
is enabled in production. Do not route QA messages to Champions' live server,
start a live collector, expose bot tokens, or use live Stripe payment keys.

## Invariants

Watch is an explicitly requested summary of **already persisted** ADM
observations, never a cheat score, detection finding or automatic enforcement.
A HTTP 202 response means a durable outbox row exists, not that Discord
accepted or displayed a message. The outbox admits one request per bound
installation/server in a rolling hour across replicas. The dedicated worker
revalidates the requester, paid entitlement, original server and exact
ADMIN_ALERTS destination. Every message carries a visible
`CHAMPION-CASE-WATCH-<deliveryId>` field. Never treat this reference as a
credential or evidence that a human authored a legitimate receipt.

Pre-send failures are BLOCKED or retried with a bounded lease. Once
SENDING is persisted, a missing Discord acknowledgement is UNKNOWN and
**cannot** be automatically retried. SENT requires a Discord message ID
returned by the API or an independently fetched, bot-authored exact-match
message verified by the owner-only reconciliation route.

## Isolated dry run (no real Discord account)

Use `go test ./internal/discord` to exercise discordgo's real REST
send/get serialization against the local `httptest` server, plus the
privacy/reference tests. Use disposable PostgreSQL and run
`go test -tags integration -p 1 -count=1 ./internal/app` with
`ALLOW_INTEGRATION_DB_TESTS=true`, `REQUIRE_INTEGRATION_DB=1` and
`TEST_DATABASE_URL` pointing **only** at the disposable database.
The integration tests exercise competing replicas, paid/revoked access,
scope and staff route, original requester revocation, private-channel
refusal, lease fencing and receipt reconciliation after an ambiguous send.
None sends a message to real Discord.

## Future operator-approved staging guild test

1. Use a separate QA Discord guild and channel. Explicitly deny @everyone
   View Channel on the target channel. Grant only approved management roles
   and the Champion QA bot, which needs View Channel, Send Messages and Embed
   Links. Verify the actual human role membership manually; a technical
   permission check cannot prove staff vetting.
2. Use a disposable staging installation and test DB, a separate QA bot
   credential stored in staging environment variables, and an explicitly
   test-mode Stripe account. Keep all production flags unchanged. Never
   repoint the live Champions installation for a test.
3. Populate synthetic or consented test ADM observations, configure the
   exact ADMIN_ALERTS route in the QA guild, and make ONE manual Watch
   request. Capture only the delivery ID, timestamp, server ID and status
   response; no bot token, keys or personal player data.
4. Check the receipt. If SENT, verify the returned Discord message ID points
   to the QA channel and has the exact reference in the embed; confirm
   only one message was sent. Check the footer explicitly says observation
   only and no enforcement. If the route becomes public, the requester loses
   permission, the subscription lapses or the selected server changes before
   dispatch, no message should send. Rechecking receipt must not resend.
5. Deliberately simulate a lost API acknowledgement in the QA transport.
   Confirm UNKNOWN never requeues or sends a duplicate on another replica
   or after restarting the process.

## UNKNOWN: human reconciliation, not retry

First read the scoped receipt via
`GET .../admin/anti-cheat/premium/watch-digest/{deliveryID}`. Verify that
status is UNKNOWN and record the saved attempted channel ID. Open that
**exact original** Discord staff channel; do not rely on a screenshot,
public message or a message in a replacement channel. Search its history
for the visible `CHAMPION-CASE-WATCH-<deliveryID>` reference. If found,
copy the actual Discord message ID using Discord's Copy Message ID. The
organization owner may then request
`POST .../admin/anti-cheat/premium/watch-digest/{deliveryID}/reconcile`
with the JSON body `{"messageId":"<Discord message ID>"}` through the
authenticated backend. The backend itself reads that exact message from
Discord and verifies the bot author, saved channel, exact digest title and
exact delivery reference; its compare-and-swap changes only UNKNOWN to
SENT and audits the action. It **never sends** a message or activates
billing. A stale receipt, other installation/server, human-authored message,
mismatched reference or unverified Discord response must be rejected.

If no matching message can be verified, leave UNKNOWN. Lack of a visible
message is not proof of non-delivery (history, permissions and Discord
retention can obscure it). Do not manually edit the database to SENT, delete
the row, or requeue it. Escalate with the delivery ID, original channel,
UTC attempt window and sanitized diagnostics for operator review. A new,
separate request after cooldown requires an explicit operator decision
acknowledging that the earlier request could have delivered; it is never an
automatic retry of the UNKNOWN row.

## Exit criteria before any customer-facing activation

- Current draft backend + website CI passes at exact reviewed commit heads.
- Disposable DB migration 0059/0060 and real REST stub regressions pass.
- QA bot and test guild channel privacy reviewed; one actual QA Discord
  delivery and lost-ACK case observed, without touching live Champion routes.
- No duplicate sends across restart/replicas; receipts and audit rows checked.
- Separate Stripe sandbox is verified (`livemode=false`), webhook QA and
  outstanding billing gates completed, then an operator approves release.
