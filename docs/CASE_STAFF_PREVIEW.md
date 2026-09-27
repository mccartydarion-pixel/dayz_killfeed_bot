# C.A.S.E. — synthetic staff card preview

This branch is stacked on Phase 2G.5 draft PR #113 and addresses issue #115. Its only renderer is `discord.BuildCASEStaffDemoEmbed()`, a fixed Go fixture. It takes **no player, source, server, finding, or credential input**, creates a fresh embed on demand, and uses a fixed fixture timestamp. There is no publisher wiring, route, new endpoint, runtime command, scheduler, webhook, or send call in this change. The separate offline CLI `go run ./cmd/case-staff-preview` only prints this immutable fixture as JSON to stdout; it has no network or Discord send path. It does not create a C.A.S.E. case or risk score.

The card says **SYNTHETIC PREVIEW — NOT A REAL PLAYER OR DETECTION** and reports the only defensible current detector state: `CASE-MOV-001 BLOCKED`, safe speed pairs zero and enforcement disabled. No accusation, player identity, private location, or false source-coverage claim is shown.

This is not a live staff notification and should not be sent to the live Champions Discord as a cheating alert. Existing `ADMIN_ALERTS` operational alerts do not become C.A.S.E. cheating findings. Real finding-based notifications need independent detector validation, authorization, routing, deduplication, privacy and review. Real Champions source coverage remains unverified under issue #114. Do not merge this branch ahead of PRs #112 and #113 or bypass their release gates.


## Future reviewed-finding delivery contract — design only

Nothing in this section is implemented or authorized to send. The fixed synthetic preview cannot be used as a live alert, and BLOCKED diagnostics are never accusations.

**Eligibility.** A future notification requires an independently validated/enabled detector, a reviewed evidence-linked finding, accepted current-source provenance, bounded observation window, trusted event timing, sufficient role-specific coordinates when relevant, tested exception/false-positive handling, and an audited human review state. Missing or ambiguous prerequisites fail closed into private diagnostics. Retained-event counts, a healthy worker, or passing CI do not establish movement detection. CASE-MOV-001 stays ineligible until gameplay elapsed time and continuous movement sampling are validated.

**Minimal immutable internal event.** Include schema version; guild/organization and exact installation/server scope; opaque finding and evidence references; detector ID/version; finding type and review state; source-quality snapshot reference; observation window and explicit coverage limitations; generation timestamp; idempotency key; privacy classification. Resolve authorization again at delivery and link opening. Do not expose ADM paths, credentials, player coordinates, fabricated speed/confidence, or unescaped mentions.

**Delivery rules.** Use a separately enabled, per-installation private staff destination; default off. If the route is missing or unauthorized, record non-delivery with no fallback to killfeed, announcements, public chat or another server. Existing ADMIN_ALERTS operational alerts are not cheating findings. Require authorization and eligibility before enqueuing and before sending. A durable outbox with a unique tenant/installation/finding/event-version key must dedupe concurrent retries, bound backoff, record failures and suppression/resolution reasons, and prevent cross-server disclosure. Neutralize Discord mentions and escape user-controlled text. Send only a minimal neutral summary such as “finding awaiting staff review” and an independently authorized evidence view. Notification alone never bans, kicks or sanctions.

**Acceptance tests for a future publisher PR.** Default-off and BLOCKED no-send; missing/stale/mismatched source no-send; forbidden or missing destination no fallback; guild/installation and reviewer permissions; cross-server isolation; retry/concurrency deduplication; mention escaping; failed-send audit and bounded retries; revoked evidence link; suppression; rollback; and unchanged live killfeed. First satisfy #114 with separately authorized, staff-only real Champions source readback and independent detector validity review. Then review a distinct implementation and private-channel test before separately approving live findings.

**Sequence:** review #112 and #113, approve a separate read-only rollout for #114, validate detector prerequisites and false positives, review publisher design and private QA, and only then consider real finding-based notifications. This preview's green CI bypasses none of those gates.


## Operational publisher boundary (draft implementation)

The existing `ADMIN_ALERTS` publisher now explicitly admits only its six defined operational kinds at both `Publish` and `send`. Unknown and C.A.S.E. diagnostic/preview kinds are dropped, including if directly inserted into its queue. A regression verifies non-delivery and preserves delivery of ADM_STALE. This is defensive separation, **not** a finding notification system: it defines no C.A.S.E. event, route, outbox, scheduler, or send path. Review the impact on all existing operational callers and the latest exact-head CI before merging. Production is unchanged until an approved release.

## Offline visual inspection

Run `go run ./cmd/case-staff-preview` from the repository checkout to print an indented Discord-compatible embed JSON object to stdout. This is a local-only design export. It does **not** post to Discord, contact the API, accept a player or server, read the database, or fetch secrets. It is intentionally the fixed `DEMO ONLY` card, not a sample live accusation. Its unit test checks deterministic JSON output, synthetic labelling, `BLOCKED`, `DISABLED`, and absence of mass mentions/webhook URLs. Reviewing this output is independent of any live evidence acceptance gate.

## /setup C.A.S.E. category (draft)

The shared Channel System V2 planner used by Discord `/setup run|repair`, website auto-setup and repair now provisions a separate `🔒 CHAMPION • C.A.S.E.` category with `🛡️・case-status`, `📁・case-evidence`, and `🚨・case-alerts`. Each channel gets one explicit informational starter card; repeated repair reuses the exact managed routes/channels and does not repost that starter when bot content remains. User-managed route choices remain preserved by repair; existing channels are never automatically deleted. The C.A.S.E. category is hidden from `@everyone` on creation, with the bot's own view/send permissions. Staff without Administrator permissions may need explicit access granted by the guild owner.

These are **setup-only informational destinations**, not functioning detector feeds: `case-status` explains readiness in the authenticated dashboard, `case-evidence` points staff to the protected evidence view without copying private content, and `case-alerts` visibly says `NOT ENABLED`. No runtime producer reads or sends the C.A.S.E. route keys. In particular, a route mapping and visible starter card do not establish an operational C.A.S.E. notification service or authorize posting the synthetic demo card as a cheating report. Existing `ADMIN_ALERTS` remains exclusively operational.

Privacy: auto-setup refuses to reuse a same-named public C.A.S.E. category or a C.A.S.E. channel with an explicit `@everyone` view override. Instead it creates a private managed destination, leaving existing customer channels untouched. Repeat setup must be idempotent. Before future real alerts, separately verify channel permissions/role access, route isolation and the reviewed-finding delivery gates in #114; no alert is enabled by this layout change. This is draft code, not deployed Discord configuration.
