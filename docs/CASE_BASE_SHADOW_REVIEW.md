# C.A.S.E. Base Boosting — shadow review

The Base Boosting test run (`GET …/admin/case/bases/shadow`, server owner only) runs the
Base Boosting detector over this server's newest 500 retained **Placed/Built** ADM lines and its
registered bases. It only observes. It records nothing, never notifies and never enforces, and
the detector stays `BLOCKED` until this review passes (see `docs/CASE_STAFF_ALERTS.md` for the release step).

## What it needs

| Need | How it is met |
| --- | --- |
| Build lines in the ADM | Nitrado: turn on `adminLogBuildActions` (and `adminLogPlacement`). |
| Build lines retained by C.A.S.E. | `CASE_EVIDENCE_ENABLED` **and** `CASE_BUILD_EVIDENCE_ENABLED` for the server, via env or the admin feature flag "C.A.S.E. build evidence". Without both, the run says "Not enough data". |
| Trusted time | The server's UTC offset, learned from `restart.log` (same as Suspicious Logins). |
| Registered bases | Bases tab. Owner-registered, non-withdrawn bases count as verified for this test run. Only builds **after** a base was registered count. |
| Current ADM feed | While the feed is stale the run says "Paused". |

## What counts

A finding is a **Placed** or **Built** line inside a base circle by someone who is not the owner,
not on the base's friend list (active player grant), and not in an allowed faction (owner's
faction or an active faction grant). Dismantling is not boosting; the Base Raid Alarm covers that.
Faction membership is each player's **current** faction, not the faction at build time.

## Review

For about two weeks, open **Anti-Cheat → Bases** and record every finding in the test card:
date, base, player, what was built, and whether it had a normal reason (a friend missing from
the friend list, a faction change, a wrong base circle) or is unexplained. Fix registrations as
you go (radius, friends). Release needs few false findings at default settings, owner-approved
thresholds, and separate owner approval.
