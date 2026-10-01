# Champion Shop — Guarded Nitrado File Write (Gate A tool)

This is a single-file, owner-authorized Nitrado write for the Shop canary.

**Only Gate A is active:** create the empty Champion spawner file, `champion/champion_shop_delivery.json`. Gate E (stage) and Gate G (unstage) are reserved names that the code refuses.

**Nothing here runs in the bot.** The capability is a command-line tool, `cmd/shop-mission-write`, that an operator runs with the owner's approval. Bot startup, Live Sync, Shop delivery and the canary operator API never import it (enforced by `TestWriteCapabilityIsIsolated`).

**Nothing in this change has written to a production server.**

## 1. Upload protocol

Source: Nitrado's official PHP SDK, `github.com/nitrado/NitrAPI-PHP`, `lib/Nitrapi/Services/Gameservers/FileServer/FileServer.php` (`uploadToken`, `writeFile`, `createDirectory`), and its HTTP client (`dataPost` sends form parameters).

| Step | Request | Response |
|---|---|---|
| 1. Upload token | `POST /services/{id}/gameservers/file_server/upload`, form-encoded `path=<directory>`, `file=<file name>`, bearer API token | `{"data":{"token":{"url":"<file-server URL>","token":"<single-use token>"}}}` |
| 2. Transfer | `POST <url>`, header `token: <token>`, `Content-Type: application/binary`, body = the raw bytes | 2xx. The SDK documents that an existing file is **overwritten**. |
| mkdir | `POST /services/{id}/gameservers/file_server/mkdir`, form-encoded `path=<parent>`, `name=<directory>` | 2xx |

The public API reference and community reports give no more detail. For example, [NitrAPI issue #24](https://github.com/nitrado/NitrAPI/issues/24) asks how to use the upload URL and token, and has no answer. The SDK is therefore the reference. **None of these calls has been made against a live server** (section 6).

Implementation (`internal/nitrado/file_write.go`):

* `RequestUploadToken`, `PostUpload` and `Mkdir` each send **exactly one** request: no retry, not even on 429 or 5xx, because a write must never repeat itself.
* Redirects are not followed, so the token header cannot travel to another host.
* **Trust boundary.** The upload destination returned by the API must meet all of these:
  * `https`, on the default port, with no user-info;
  * its host is `nitrado.net` or a subdomain of it (`nitrado.TrustedUploadDomain`). IP literals and look-alikes such as `nitrado.net.example.com` or `evilnitrado.net` are refused.

  The check runs when the token response is parsed and again inside `PostUpload`. A destination outside the boundary is never contacted, and the token is dropped. The dry run reports the host of a signed **download** URL (a read) and whether it is inside the boundary, so the owner sees before approval whether Nitrado's file server qualifies.
* The `token` header is sent only to the file-server URL. The API bearer is never sent there.
* `UploadTarget` keeps the URL and token in unexported fields. `String`, `GoString` and `MarshalJSON` redact them. Errors (`WriteError`) carry a phase, a kind and an HTTP status only: never a URL, a token or a response body.
* `ListEntries` lists files **and directories**. `ListDir` returns files only, so it cannot tell an absent directory from an empty one.

## 2. Single-operation safety (`internal/shop/missionwrite`)

A write happens only when **all** of the following hold.

| Requirement | Enforcement |
|---|---|
| Explicit operation | `-operation gate-a-create-empty`. Unknown operations are refused; `gate-e-stage` and `gate-g-unstage` return `ErrOperationNotActive`. |
| Exact binding | Service, organization, installation and game server are checked through capability discovery, which refuses a service that is not the bound one. The canonical mission path must equal `-mission`. |
| Exact destination | `-path` must equal the operation's single fixed path. `ValidateRelPath` refuses the following: <ul><li>absolute paths;</li><li>`..`, `.` or empty segments;</li><li>backslash, NUL, spaces, `%`, `:` and globs;</li><li>anything that is not a `.json` file.</li></ul> Physical paths are built only with `capability.SafePath` inside the account root. |
| Expected current state | `-expect-current absent`. Gate A never overwrites: a present file, a directory in its place, or a file named `champion` are all refused. The state comes from directory listings, never from a failed download. |
| Expected configuration | `-expect-config-sha256` must equal the live `cfggameplay.json` SHA-256. `-expect-spawners` must equal the live `objectSpawnersArr` exactly, which keeps the Lost City reference and every other entry. |
| Expected payload | The payload is **built in**: `canary.EmptyArtifact()`, 20 bytes, LF, no BOM. It is never read from a file. `-expect-payload-sha256` must equal `328c4d64…dc5f`. CRLF, a BOM or any other content is refused. |
| Single-use authorization | The dry run prints a **plan ID**. It is a digest of the operation, binding, mission, path, current state, parent-directory state, config SHA-256, spawner list and payload. Execution requires `-execute -authorize <plan ID>` and recomputes the ID from a **fresh** inspection, so any change since approval refuses the write. The journal marks the ID as used **before** any write, so it can never run twice. |
| No write after uncertainty | If the last journal entry for the destination is `STARTED` (interrupted) or `UNCERTAIN`, every further write is refused until the owner records a resolution (`-resolve <plan ID> -note "…"`). Resolving writes to the journal only. |
| No directory recursion | Only one named file is written. `Mkdir` creates one directory level, only when Gate A's plan says the `champion/` directory is absent. |

**Journal durability**

The journal is the only local memory of which authorizations were used, so it must not be resettable by accident.

| Risk | Protection |
|---|---|
| Different working directory, or repeated invocations | The journal and anchor paths must be **absolute** and in different directories. By default the journal is in the user config directory (`%APPDATA%\champion-shop\mission-write-journal.jsonl`) and the anchor in the home directory (`%USERPROFILE%\.champion-shop-mission-write.anchor`). |
| Missing journal | Opening never creates a journal. A missing journal with an existing anchor is refused (`ErrJournalAnchor`). A brand-new journal needs an explicit `-init-journal`, which refuses if either file exists. |
| Replaced journal | The genesis line carries a random journal ID, recorded in the anchor. A different journal does not match and is refused. |
| Truncated or edited journal | Every line carries its sequence number and the SHA-256 of the previous line, and the anchor records the last sequence and hash. Removing, editing or reordering lines is refused (`ErrJournalCorrupt` / `ErrJournalAnchor`). |
| Torn write (crash mid-line) | A final line without a newline is refused. |
| Crash between the journal fsync and the anchor update | The only accepted mismatch: the journal is exactly one valid, chained entry ahead. The anchor is rolled forward. Anything more is refused. |
| Lost anchor | Refused. `-adopt-journal` is an explicit owner action that re-anchors an intact journal. It changes no entry, so an outstanding write stays outstanding. |
| Concurrent runs | An exclusive `<journal>.lock` file is held for the whole run. A second run is refused. A lock left by a crash is never removed automatically; the owner inspects, then deletes it. |
| Reuse after `UNCERTAIN` | The destination stays blocked across restarts until `-resolve`. The plan ID is **never** reusable, even after the resolution, because its `STARTED` entry is permanent. |

The journal is local, so a second machine has its own journal. The server state is the backstop: Gate A requires the destination to be absent, so after any write that reached the server, every new plan is refused.

**Execution sequence**

1. Take the exclusive journal lock. Refuse if the journal fails verification, the destination is outstanding, or the authorization was already used.
2. Run a fresh inspection. It must match every expectation, and its plan ID must equal the authorization.
3. Journal `STARTED`.
4. If the plan says `champion/` is absent, `mkdir champion` once. **Any mkdir error stops the run** (`NOT_WRITTEN`, no token requested), and so does a success that is not followed by a listed folder. The folder's resulting state is reported as a side effect: `created`, `not created`, `present after a mkdir error`, or `unknown`.
5. **Re-verify immediately before writing:** the destination is still absent and `cfggameplay.json` is unchanged. Otherwise the tool aborts with `NOT_WRITTEN` and requests no token.
6. Request the upload token. The URL must pass the trust boundary. On failure, nothing is sent, but the read-back still runs, because the token request itself is a server-side call whose side effects are undocumented.
7. Send one transfer. Its result is recorded, but it does not decide the outcome.
8. **Independent read-back** (list plus download) decides the outcome:

| Read-back | Outcome |
|---|---|
| Exactly 20 bytes with SHA-256 `328c4d64…`, after a successful token request | `WRITTEN_VERIFIED`, even if the transfer response was lost |
| Absent, and the token request or the transfer was refused | `NOT_WRITTEN` |
| Absent, but the transfer reported success | `UNCERTAIN` |
| Any other content (partial, foreign, or a file that appeared after a refused token request) | `UNCERTAIN` |
| The read-back fails | `UNCERTAIN` |

9. **Post-write checks:**
   * `cfggameplay.json` still has the expected SHA-256;
   * no restart: the newest `DayZServer_*.ADM` and the gameserver status are unchanged.
10. Journal the outcome, including the folder side effect. Release the lock. **`UNCERTAIN` means stop: no retry.**

Exit codes:

| Code | Meaning |
|---|---|
| 0 | Verified, or a dry run completed |
| 1 | Refused; nothing was attempted |
| 2 | `NOT_WRITTEN` |
| 3 | `UNCERTAIN` |

## 3. Tests (disposable Nitrado stand-in)

`internal/shop/missionwrite/standin_test.go` is an `httptest` server that implements every endpoint the real `nitrado.Client` uses:
* service, gameserver and token facts;
* `file_server/list` and signed `download`;
* `file_server/upload` with its file-server transfer;
* `file_server/mkdir`.

It works over an in-memory file tree, with failure switches. The tests drive the **real client over HTTP**.

| Scenario | Test |
|---|---|
| Successful two-step upload: exact form fields, `token` header, `application/binary`, exact 20 bytes, mkdir, read-back, config unchanged, no restart | `TestGateASuccessfulTwoStepUpload`, `TestExistingParentDirectoryIsNotRecreated` |
| Permission denied (403 on the token request) | `TestPermissionDenied` |
| Expired upload token (401 on the transfer; not retried) | `TestExpiredUploadToken` |
| Incorrect destination, mission, service or binding; reserved operations; an `http` upload URL | `TestIncorrectDestinationAndBinding` |
| Path traversal and ambiguous paths | `TestPathTraversalRejected` |
| File already exists (empty or not), a directory in place, a file named `champion` | `TestFileAlreadyExists` |
| Changed configuration hash: wrong expectation, spawner list changed, changed after approval (stale plan ID), changed mid-run (aborted before the token) | `TestChangedConfigurationHash` |
| Partial upload leads to `UNCERTAIN`, blocks further writes, and needs an owner resolution | `TestPartialUploadIsUncertainAndBlocks` |
| Read-back mismatch | `TestReadBackMismatch`, `TestReportedSuccessButAbsentIsUncertain` |
| Network interruption: dropped after storing (verified), dropped before storing (`NOT_WRITTEN`), read-back failing (`UNCERTAIN`) | `TestNetworkInterruption` |
| Duplicate execution: same authorization refused; a new plan refused because the file exists | `TestDuplicateExecution` |
| Safe refusal after uncertain outcomes or a crash mid-run | `TestPartialUploadIsUncertainAndBlocks`, `TestInterruptedRunBlocksUntilResolved` |
| Authorization missing or wrong | `TestAuthorizationRequired` |
| Payload exactness: CRLF, BOM, other content, wrong or missing hash | `TestPayloadIsExactlyTheEmptyFile` |
| A restart during the window is reported | `TestRestartDuringWriteIsReported` |
| No token, upload URL, API bearer, signed URL or account path in any output, the journal or errors | `TestNoSecretInAnyOutput` |
| Protocol layer: redaction, form fields, one request only, no redirect, https-only, sanitized network errors, one-level mkdir | `internal/nitrado/file_write_test.go` |
| Isolation: only this package calls the write primitives, only `cmd/shop-mission-write` imports it, only tests relax https | `TestWriteCapabilityIsIsolated` |
| Upload trust boundary: https, default port, `*.nitrado.net` only, no IP literals, look-alikes or user-info; a refused destination is never contacted | `TestUploadTrustBoundary` |
| Journal: absolute and separate paths; restart and working-directory independence; no silent re-creation; replaced, truncated, edited, torn or garbage journal refused; lost anchor needs explicit adoption, which keeps outstanding writes | `TestJournalPathsMustBeAbsoluteAndSeparate`, `TestJournalSurvivesRestartAndWorkingDirectory`, `TestMissingJournalCannotResetAuthorization`, `TestReplacedTruncatedEditedOrTornJournalIsRefused` |
| Crash between the journal and anchor writes: one entry behind recovers, more is refused | `TestAnchorOneBehindIsRecoveredTwoBehindRefused` |
| Concurrent runs serialized; a stale lock is never taken over | `TestConcurrentRunsAreSerializedAndStaleLocksKept` |
| After `UNCERTAIN`: refused after a restart, and the plan ID stays dead even after the owner's resolution | `TestUncertainAuthorizationNeverReusableAfterRestart` |
| Failure side effects: mkdir refused, mkdir error with the folder created, mkdir ok then token refused, token refused but a file appeared, transfer refused; the folder state is in the outcome and the journal | `TestFailureSideEffectsAreReported` |

## 4. Gate E and Gate G (not active)

`missionwrite.OpStageItem` and `OpUnstageItem` are reserved and refused. Activating them is a separate, owner-approved change. It must add, per operation:

* **Gate E:**
  * payload = `canary.PreviewSingleItem` for the real delivery;
  * expected current = the empty file (`328c4d64…`);
  * the ledger attempt must be `FILE_PREPARED`, and its prepared SHA-256 must equal the payload;
  * `CheckStagingReadiness`.
* **Gate G:**
  * payload = the empty file;
  * expected current = the staged SHA-256 recorded on the attempt;
  * the attempt must be `UNSTAGE_REQUIRED`.
* Recording the ledger transition only after a `WRITTEN_VERIFIED` read-back.

The upload, read-back, plan-ID and journal machinery above is reused unchanged.

## 5. Security safeguards (summary)

* No production write in development or CI.
* The write capability is isolated from the bot.
* Dry run is the default.
* Execution needs a plan-bound, single-use owner authorization.
* No retry, no redirect; the token goes only to https `*.nitrado.net`.
* The journal is tamper-evident, anchored and locked, and never silently re-created.
* Any mkdir error stops the run, and folder side effects are reported.
* The payload is built in and hash-checked.
* Gate A never overwrites.
* Verification is by independent read-back.
* `UNCERTAIN` stops everything until the owner resolves it.
* Tokens, signed URLs, the API bearer and the physical account path are never printed, journaled or returned in errors.
* `cfggameplay.json` is never written.
* No restart call exists in this code.

## 6. Remaining Nitrado API uncertainties

1. **The upload flow has never been exercised live.** It is taken from Nitrado's own SDK, not from a live response. Gate A is the first real use, so write capability stays `UNVERIFIED` until its read-back passes.
2. **Write permission on this service.** The service lists `ROLE_WEBINTERFACE_FILEBROWSER_WRITE`, but that is documentation-level evidence. Whether the console service accepts writes into the `noftp` mission folder is unknown. A 403 is handled (`NOT_WRITTEN`).
3. **Directory creation.** Whether an upload into a missing directory creates it is unknown, so the tool never relies on it: it runs an explicit, single `mkdir` first. How `mkdir` answers for an existing directory is also unknown, so the tool calls it only when the listing shows the directory absent.
4. **Response codes.** The exact success code and body of the transfer and `mkdir` are undocumented. The tool accepts any 2xx and decides by read-back.
5. **Token lifetime** is undocumented. The token is used immediately, once.
6. **Read-after-write consistency** of `list` and `download` is unverified. A lagging listing would show as `UNCERTAIN`, which is the safe side, and the owner re-inspects.
7. **No conditional write.** Nitrado has no If-Match. The re-read immediately before the token request narrows the race to seconds, and the read-back detects anything else.
8. **The file-server host.** Whether upload URLs are under `nitrado.net` is unverified. The dry run shows the download host, but the upload host could differ. If it is outside the boundary, the run ends `NOT_WRITTEN` without contacting it, and one token request will have been spent.
9. **Side effects of the token request.** Undocumented. The tool reads back after a refused token request too; a file that appeared is reported as `UNCERTAIN`.

## 7. Gate A execution plan

Each step is separate. **Steps 4 and 5 need explicit owner approval.** Steps 1–3 write nothing to the server.

1. **Review this PR.** Running the tool from its branch is not a deployment. The bot does not include it.
2. **Create the journal once** (local files only, no server call). It uses the default absolute locations: the journal in `%APPDATA%\champion-shop\`, the anchor in `%USERPROFILE%`.

   ```
   go run ./cmd/shop-mission-write -init-journal
   ```

   Keep both files. Do not move, edit or delete them.
3. **Dry run (read-only).** Run it from the repository folder:

   ```
   railway run -s dayz_killfeed_bot go run ./cmd/shop-mission-write -operation gate-a-create-empty -service 19806451 -org 1 -installation 11 -game-server 1 -mission dayzps_missions/dayzOffline.chernarusplus -path champion/champion_shop_delivery.json -expect-current absent -expect-config-sha256 4d000807963a618ed5e6c50a3f3c3a90247829ff3f3674d0856ee3cfc69200aa -expect-spawners '["custom/The_Lost_City.json"]' -expect-payload-sha256 328c4d64bb81bdbdddad2431a12b6197182f5fa5646bf16c7e0e8d437bc8dc5f
   ```

   It prints:
   * the fresh inspection: mission, `cfggameplay.json` size and SHA-256, spawners, `champion/` present or absent, destination absent, gameserver status, current boot;
   * the **file-server host**, and whether it is inside the upload trust boundary;
   * the journal state;
   * the steps;
   * the **plan ID**.

   If `cfggameplay.json` changed since Gate 0, the dry run refuses. A new reading is then needed; old hashes are never reused. If the file-server host is outside `*.nitrado.net`, stop: the upload would be refused, and widening the boundary is a reviewed code change.
4. **Owner approval of that exact plan ID.**
5. **Execute:** the same command plus `-execute -authorize <plan ID>`, at a time with no restart expected in the next few minutes. The tool re-inspects, creates `champion/` if needed, uploads 20 bytes once and reads them back.
6. **Result:**

| Status | Meaning | Next step |
|---|---|---|
| `WRITTEN_VERIFIED` | Read-back 20 bytes, `328c4d64…`; config unchanged; no restart | Gate A done; write capability VERIFIED LIVE |
| `NOT_WRITTEN` | The file is not on the server. The `directory:` line says whether `champion/` was created as a side effect. | Report the check that failed; no automatic retry; a new approval is needed (the plan ID changes if the folder now exists) |
| `UNCERTAIN` | A write may have happened | Stop. Inspect read-only (`shop-canary-prepare`). The owner decides and records the resolution (`-resolve <plan ID> -note "…" -service 19806451 -path champion/champion_shop_delivery.json`). |

7. **Independent confirmation:**
   * `shop-canary-prepare` shows the Gate A line "present, SHA-256 `328c4d64…`", and `cfggameplay.json` is still `4d000807…`;
   * `shop-ledger-verify -phase 0055` still passes: 0 attempts, lock closed.

Rollback: none needed. An unreferenced empty file is never read by the server. If the owner wants it gone, delete it in the Nitrado file browser.
