# Base Boost Detection — evidence feasibility

This is one of the approved Core Eight. It means unauthorized construction inside or around another player's registered base. It does not mean kill farming.

## What exists

- `internal/killfeed/parser.go` parses player `placed`, `built` and `dismantled` ADM lines with a subject and optional position when `adminLogPlacement` or `adminLogBuildActions` is enabled on the DayZ server.
- The existing `BUILD_FEED` publishes parsed, deduplicated build activity to the staff route. Its documentation explicitly says no real ADM build-line sample is in this repository; parser tests use fixtures.
- `internal/killfeed/evidence.go` persists selected combat and lifecycle lines to the installation-scoped, source-addressed C.A.S.E. evidence ledger when its separate collector allowlist is enabled. `BUILD_ACTION` is not a candidate. The staff build feed does not retain a C.A.S.E. source record or a durable action/part/target/tool tuple.

## Why the detector stays blocked

A staff build card proves neither ownership nor unauthorized entry. It is not a durable, source-addressed C.A.S.E. record. A placement position without a verified base zone, identity, and time-specific member/faction permissions cannot establish a violation. A missing build line cannot prove that no building occurred when server flags, polling, or coverage are unknown. Existing connect/hit/kill positions cannot substitute for a construction action.

## Next isolated implementation

1. Obtain a representative real ADM build line from an authorized test server, with placement/build flags independently verified. Confirm parser fields and coordinate order; record the exact source address, not an inferred gameplay timestamp.
2. Extend the opt-in C.A.S.E. evidence sink to retain build action, part, target and tool as bounded structured fields, scoped by guild/server/source/offset/hash. Review the schema and replay/collision behavior in disposable PostgreSQL first. Do not change the production schema or server flags without separate approval.
3. Read registered base zones and effective owner/faction permissions at the event's validated context. Define explicit missing-registration, guest, raid-window, overlapping-zone, custom-map and staff/admin exclusions.
4. Correlate repeated independent actions only after source freshness, identity, zone and permissions are verified. An observation remains neutral; staff case notification requires later release gates. Deduplicate incident delivery through the existing case outbox.

No detector, private cheating alert or enforcement is enabled by this feasibility note.
