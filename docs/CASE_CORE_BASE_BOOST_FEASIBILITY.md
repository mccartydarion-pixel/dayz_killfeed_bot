# Base Boost Detection — evidence feasibility

This is one of the approved Core Eight. It means unauthorized construction inside or around another player's registered base. It does not mean kill farming.

## What exists

- `internal/killfeed/parser.go` parses player `placed`, `built` and `dismantled` ADM lines with a subject and optional position when `adminLogPlacement` or `adminLogBuildActions` is enabled on the DayZ server.
- The existing `BUILD_FEED` publishes parsed, deduplicated build activity to the staff route. Its documentation explicitly says no real ADM build-line sample is in this repository; parser tests use fixtures.
- The opt-in C.A.S.E. evidence collector now includes `BUILD_ACTION`; migration `0064_case_build_evidence` adds bounded action/object/target/tool columns to the existing source-addressed observation. The repository checks the tuple on source replay and the scoped read returns it with any actual player position. The staff build feed remains on its existing path. This is branch code only; no production migration or collector activation has been authorized.

## Why the detector stays blocked

A staff build card proves neither ownership nor unauthorized entry. A parsed source-addressed build observation still cannot establish those facts. A placement position without a verified base zone, identity, and time-specific member/faction permissions cannot establish a violation. A missing build line cannot prove that no building occurred when server flags, polling, or coverage are unknown. Existing connect/hit/kill positions cannot substitute for a construction action.

## Existing zone and faction data

`installation_zones` includes a `BASE_RADAR` type with a center, radius, and authorization/ignore entries. It does not record a player-owned registered base or a time-specific ownership claim. A staff-created zone, alert route, or authorization entry must not be treated as a base registration. Faction membership has history, but the build line's ADM clock has no trusted event date; current membership cannot prove permission at the time of construction. Overlapping zones must remain ambiguous until an explicit rule and verified registrations exist.

## Next isolated implementation

1. Obtain a representative real ADM build line from an authorized test server, with placement/build flags independently verified. Confirm parser fields and coordinate order; record the exact source address, not an inferred gameplay timestamp.
2. Review build evidence migration, replay/collision and server isolation in disposable PostgreSQL; keep it unmerged until production schema authorization. Do not change the production schema or server flags without separate approval.
3. Read registered base zones and effective owner/faction permissions at the event's validated context. Define explicit missing-registration, guest, raid-window, overlapping-zone, custom-map and staff/admin exclusions.
4. Correlate repeated independent actions only after source freshness, identity, zone and permissions are verified. An observation remains neutral; staff case notification requires later release gates. Deduplicate incident delivery through the existing case outbox.

No detector, private cheating alert or enforcement is enabled by this feasibility note.
