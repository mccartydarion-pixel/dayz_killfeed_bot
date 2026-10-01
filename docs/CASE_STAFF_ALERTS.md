# C.A.S.E. staff alerts

Real C.A.S.E. alerts post the staff card (`discord.BuildCASECore8StaffEmbed`)
to the installation's private `CASE_ALERTS` channel. **Nothing is sent today**:
no detector is released, and every owner's switch starts off.

## When an alert is sent

All of these must be true:

1. **The detector is released.** `caseintel.ModuleReleased(id)` is true only
   when the detector is `VALIDATED_SHADOW` in `ClientCatalog` **and** has
   approved thresholds in `releasedThresholds` (`internal/caseintel/core8_release.go`).
   Today only Suspicious Logins (`CASE-LOGIN-001`) is wired, and it isn't released.
2. **The owner turned staff alerts on** for that server (dashboard switch, `case_alert_settings`).
3. **The finding may notify.** The detector returned `CanNotify` (released, health
   `ACTIVE`, enough independent observations for the owner's sensitivity) and the
   finding's tier is `SUSPICIOUS`.
4. **The channel is private.** Right before each send, `CASE_ALERTS` must resolve
   to a channel in the installation's guild, under a category that denies
   `@everyone`, with no public override, and the bot must be able to post there.

The card says "needs a staff look" and links to the player on the dashboard
(`CHAMPION_SITE_BASE_URL`, https only). Mentions are disabled. There are no
automatic bans, kicks or other actions.

## How it runs

`case_staff_alert_worker.go` ticks every minute:

- **Scan** (only if a detector is released): for each server with alerts on and
  C.A.S.E. evidence collection on, it checks players who connected or
  disconnected in the last 30 minutes (up to 100), using the same evidence and
  trusted-time rules as the dashboard's shadow run, and queues each new finding.
- **Deliver:** it claims up to 10 queued alerts (`case_alert_deliveries`) and
  rechecks the owner switch, the release and the channel before posting.

Each incident is queued at most once per installation (keyed by its incident
key). Failures retry after 1, 4, 9 and 16 minutes; after 5 attempts the alert
is marked `DEAD`. Turning the switch off, or un-releasing the detector, stops
alerts that are still queued (`OWNER_DISABLED`, `DETECTOR_NOT_RELEASED`).

## Releasing Suspicious Logins (after its review passes)

Follow `docs/CASE_LOGIN_SHADOW_REVIEW.md`. When the exit criteria are met and
the owner approves:

1. In `internal/caseintel/detectors.go`, set `CASE-LOGIN-001`'s `Mode` to `"VALIDATED_SHADOW"`.
2. In `internal/caseintel/core8_release.go`, add its approved thresholds:
   `"CASE-LOGIN-001": {Approved: true, MinimumEvidence: …, Relaxed: …, Balanced: …, Strict: …}`
   (Relaxed ≥ Balanced ≥ Strict ≥ MinimumEvidence).
3. Update `TestNoModuleIsReleasedWithoutReview` and `TestNoDetectorIsReleasedYet`,
   which fail on purpose until then.
4. Deploy, then the owner turns staff alerts on.

**Rollback:** turn the owner switch off (immediate), or revert step 1 or 2.

## API (server owner only)

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/admin/case/alerts/settings` | | `{settings:{enabled,updatedAt}, releasedDetectors:[…], recent:[…10], enforcement:"DISABLED"}` |
| PUT | `…/admin/case/alerts/settings` | `{enabled}` | Audited `CASE_STAFF_ALERTS_SAVED`. |

Migration `0081_case_staff_alerts` adds `case_alert_settings` and `case_alert_deliveries` (additive only).

## Relation to the older review/outbox tables

`case_review_cases` / `case_staff_outbox` (migration 0063) remain inert. They
model delivery *after* a staff review. These alerts are the prompt that asks
staff to review, so they use their own small queue keyed by the Core 8
incident key.
