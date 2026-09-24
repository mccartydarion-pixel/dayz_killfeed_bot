# C.A.S.E. Phase 2E.1 — ADM Boot-Authority Recovery

## Scope

The existing source authority is based on a verified ADM filename/header boot stamp. Do **not** promote a source based solely on filename, mtime, size, or apparent activity, and never step backward to an older accepted boot.

Discovery and the periodic boot scan used to collapse noftp/ftproot representations into one canonical candidate **before** checking its header. If the preferred noftp file appeared in the listing but could not be read, the alternate ftproot representation was discarded. This change retains physical aliases for boot-header verification, tries noftp first and then the secondary alias for the *same canonical boot*, while leaving normal ranking, canonical checkpoint identity, and no-backward-boot guards intact.

If neither alias has a matching readable header, do not promote that boot. The boot-authority snapshot now records the canonical candidate reference, last verification time, and safe failure classification (e.g. read failure, missing header, mismatched header). The existing staff-only integrity endpoint exposes this result without raw Nitrado paths or credentials. A header-only 124-byte ADM with a valid matching boot stamp **can** be accepted as current, but does not prove that gameplay is being logged.

## Verification

- Quiet verified new boot is accepted before the stale rediscovery path.
- A listed but unreadable noftp representation can fall back to the verified ftproot alias without promoting an older boot.
- If both aliases have mismatched headers, the accepted current boot remains unchanged.
- A listing gap or failed verification never causes replay of a historical ADM.
- Exact source offsets/evidence replay and existing killfeed paths are unchanged.
- Inspect live `newerBootVerificationReason` and `newerBootCandidateRef` when SOURCE_LAGGING persists. If the candidate is verified and accepted but the file never grows during gameplay, investigate the upstream Nitrado/DayZ ADM writing configuration; do not force an older file or fabricate hits.

No new polling loop, database migration, detection, scoring, or automatic enforcement. Deploy only after build, vet, unit, race and disposable-PostgreSQL CI pass.