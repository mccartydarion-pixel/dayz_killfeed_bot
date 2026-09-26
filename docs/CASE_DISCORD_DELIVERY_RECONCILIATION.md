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

## Phase 6.10 — one-shot QA Discord transport probe

A dedicated `cmd/case-discord-qa` program is available on the draft branch.
It is **not part of the server startup**, is read-only by default, and never
uses Stripe, Nitrado, live evidence, the C.A.S.E. database or live player
records. Its synthetic message has a **different title/reference** from a
real paid digest and cannot be used to reconcile one.

Before running it, prepare a separate Discord QA guild **or a dedicated
private #case-qa channel within the existing Champions guild**, plus a
separate QA bot application. Existing-guild mode requires additional guards. Obtain the public Discord snowflake
IDs for the QA guild, QA channel, QA bot user, **actual production guild**,
and **actual production bot**. The QA bot and production bot must differ. In existing-guild mode, the
guild IDs intentionally match, but the target must be an explicitly private
channel named exactly #case-qa, distinct from every declared live output
channel.
Store the QA token only in a secret manager or local environment variable
`CASE_DISCORD_QA_BOT_TOKEN`; never paste it into this chat, a PR, logs or
a command-line argument. Do not use the live Champions bot token. The bot
requires View Channel, Send Messages, Embed Links and Read Message History.
Keep the QA channel private where practical, but the synthetic transport
probe does not audit which human operators can access it. The stricter
staff-delivery gate must still pass before any real Watch payload is sent.

First run a **read-only preflight** from the backend repository using the
QA token stored in the environment:

```sh
go run ./cmd/case-discord-qa -mode preflight -guild "$QA_GUILD_ID" -channel "$QA_CHANNEL_ID" -bot "$QA_BOT_ID"
```

This fetches bot identity, current guild/channel permissions and destination
privacy. It does not send a message. To send one **synthetic** message, an
operator must supply the production denylist IDs plus two explicit approvals:

```sh
export CASE_DISCORD_QA_ALLOW_SEND=YES_ONE_SYNTHETIC_QA_MESSAGE
go run ./cmd/case-discord-qa -mode send-once \
  -guild "$QA_GUILD_ID" -channel "$QA_CHANNEL_ID" -bot "$QA_BOT_ID" \
  -production-guild "$PRODUCTION_GUILD_ID" -production-bot "$PRODUCTION_BOT_ID" \
  -confirm SEND_ONE_SYNTHETIC_QA_MESSAGE
```

The command creates a fresh `CHAMPION-CASE-QA-...` reference, sends **once**,
reads back the exact message, checks its bot author/channel/reference, and
prints nonsecret IDs. The explicit production denylist is based on
operator-supplied real IDs: verify them, do not use placeholders. If the API
result is uncertain, **do not rerun send-once** to retry that reference.

For the intentionally lost-ack exercise, add `-simulate-lost-ack` to the
explicit send command. It intentionally discards a successfully returned
message ID *after exactly one send*, then prints only the QA reference.
That tests human recovery, not an actual network timeout and not the
database worker. Find the existing message in the QA channel and verify it
read-only with:

```sh
go run ./cmd/case-discord-qa -mode verify-existing \
  -guild "$QA_GUILD_ID" -channel "$QA_CHANNEL_ID" -bot "$QA_BOT_ID" \
  -message "$EXISTING_MESSAGE_ID" -reference "$EXISTING_QA_REFERENCE"
```

### Using your existing Champions Discord server

The synthetic probe can be run in your existing Champions **Discord guild**,
but only in a newly created #case-qa private text channel. This does not
change any live killfeed/ADMIN_ALERTS route, bot settings, Nitrado server or
production billing. Add the separate QA bot to the guild and explicitly
restrict #case-qa's View Channel permission for @everyone, then grant it
only to reviewed staff and the QA bot. Keep real player data out of the
synthetic probe. The production bot is not used.

Read-only preflight with the existing guild:

```sh
go run ./cmd/case-discord-qa -mode preflight \
  -guild "$PRODUCTION_GUILD_ID" -production-guild "$PRODUCTION_GUILD_ID" \
  -channel "$QA_CHANNEL_ID" -bot "$QA_BOT_ID" -use-existing-guild
```

An actual single synthetic send requires *all* of the original approvals
plus a separate existing-guild consent and a denylist containing **every
known live output channel ID**. The QA channel ID must not appear in that
denylist:

```sh
export CASE_DISCORD_QA_ALLOW_SEND=YES_ONE_SYNTHETIC_QA_MESSAGE
export CASE_DISCORD_QA_EXISTING_GUILD=YES_EXISTING_GUILD_PRIVATE_CASE_QA
go run ./cmd/case-discord-qa -mode send-once \
  -guild "$PRODUCTION_GUILD_ID" -production-guild "$PRODUCTION_GUILD_ID" \
  -channel "$QA_CHANNEL_ID" -bot "$QA_BOT_ID" \
  -production-bot "$PRODUCTION_BOT_ID" \
  -production-channels "$PRODUCTION_CHANNEL_IDS_COMMA_SEPARATED" \
  -use-existing-guild -existing-guild-confirm USE_EXISTING_GUILD_PRIVATE_CASE_QA \
  -confirm SEND_ONE_SYNTHETIC_QA_MESSAGE
```

The probe verifies that Discord currently names the target text channel
exactly `case-qa` and that the separate QA bot can View, Send, Embed and
Read Message History. Because the probe contains zero player records, the
synthetic-only preflight does **not** audit or restrict individual human
member overrides. Actual Watch/evidence delivery retains the independent,
strict `VerifyCaseStaffChannel` privacy gate (including the human grants
and explicit @everyone deny). Operators should keep #case-qa private by
normal Discord configuration, but this temporary transport check is not
a staff-access certification. The denylist depends on
operator-supplied IDs and cannot independently prove it is complete, so
review your routes manually before approving a send. Do not add your live
bot token or production channel as a workaround.

The earlier disposable-PostgreSQL worker tests separately cover the
actual `SENDING -> UNKNOWN` and no-resend behavior. A combined end-to-end
test of the actual staging app/outbox requires its own disposable database,
QA installation, independent paid test entitlement and deliberate
operator approval; running this synthetic probe alone does **not** close
the full C.A.S.E. launch gate.

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
- Disposable DB migration 0061/0062 and real REST stub regressions pass.
- QA bot and test guild channel privacy reviewed; one actual QA Discord
  delivery and lost-ACK case observed, without touching live Champion routes.
- No duplicate sends across restart/replicas; receipts and audit rows checked.
- Separate Stripe sandbox is verified (`livemode=false`), webhook QA and
  outstanding billing gates completed, then an operator approves release.
