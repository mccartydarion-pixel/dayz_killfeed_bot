# C.A.S.E. setup checklist

One owner-facing view of what anti-cheat needs on a server, at the top of the
Anti-cheat page. Each step has a plain state and what to do. It never enables
a detector, sends an alert or acts on a player.

| Step | OK when | Otherwise |
| --- | --- | --- |
| Private staff channel | `CASE_ALERTS` resolves to a private channel (same check as real and test alerts) | "Run setup / repair layout" |
| Game log feed | ADM source `HEALTHY` | Quiet (no players), lagging, Nitrado errors or stalled, in owner words |
| Server clock | `live_sync_server_clock` has an offset | Waits for the next server restart |
| Evidence collection | The collector is attached for the server | See below |
| Staff alerts | The owner's switch is on | Switch |
| Detectors | At least one is released | "In testing by Champions" |

States: `OK`, `ACTION` (the owner can fix it), `WAIT` (happens by itself),
`LOCKED` (decided by Champions), `TESTING`.

## Evidence collection

A server collects evidence when the Owner Hub flag says so, or (without a
flag) when it is on the `CASE_EVIDENCE_SERVER_IDS` allowlist, or when the
owner turned their own switch on **and** the platform allows self-serve.

- Self-serve is off by default. Turn it on with `CASE_EVIDENCE_SELF_SERVE=true`
  (with `CASE_EVIDENCE_ENABLED=true`). Without it the owner sees "Champions
  turns this on during the test period" and the switch is refused.
- The Owner Hub flag always wins; the owner then sees "set by Champions".
- `CASE_EVIDENCE_ENABLED=false` still turns collection off for everyone.
- The collector attaches when a server's worker starts, so a change applies
  after the bot's next restart (every deploy restarts it). The checklist shows
  "starts after the next restart" until then.
- Owners' choices are read once at startup (`case_evidence_optins`, migration
  `0102_case_evidence_optins`). A failed read keeps self-serve off.
- Evidence collection writes more data and shares the ADM poller with the
  killfeed; see docs/CASE_LOGIN_SHADOW_REVIEW.md §3 for the safety check.

## API (server owner, UAV_MANAGE)

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/admin/case/setup` | | `{steps, evidence, detectors}` |
| PUT | `…/admin/case/setup/evidence` | `{enabled}` | 409 unless self-serve is on and no Owner Hub flag is set. Audited `CASE_EVIDENCE_OPTIN_SAVED`. Applies after the next restart. |
