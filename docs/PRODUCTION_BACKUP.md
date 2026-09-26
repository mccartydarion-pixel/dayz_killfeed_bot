# Champion Production Database Backup and Recovery

This is an automated, verified backup of the production PostgreSQL 18 database. You install no tools; the job runs on GitHub Actions.

It exists to protect the database before Shop migrations 0054 and 0055 (PRs #97, #98, #95) are deployed. Those PRs stay on hold until a production backup has passed verification.

## 1. What runs, where

| Item | Value |
|---|---|
| Job | `.github/workflows/production-backup.yml`, on the branch **`ops/production-backup`** only. Railway deploys `main`, so the bot and Champions are never restarted or redeployed by it. |
| Trigger | A push to that branch whose commit message contains `[backup:production]`, by the repository owner. The job also needs the owner's **approval of the `production-backup` environment**. Commits without the marker run nothing; `[backup:selftest]` runs a rehearsal on disposable databases. |
| Source | The Railway `Postgres` service (the production database) over its TCP proxy, **TLS required**, PostgreSQL 18.6, about 89 MB |
| Tools | The official `postgres:18` image: `pg_dump` and `pg_restore` 18.6, matching the server major version |
| Read-only guarantee | Every session runs with `default_transaction_read_only=on`. The snapshot session is `REPEATABLE READ READ ONLY`. `pg_dump` only reads. |
| Consistency | One exported snapshot is used for both the dump and the inventory, so the restored copy is compared with exactly what was dumped, even while the bot keeps writing. |
| Format | `pg_dump --format=custom --compress=6` (complete database: schema, data, sequences, functions, triggers) |
| Verification | Inside the job: `pg_restore --list` validates the archive. The archive is then restored with `--single-transaction --exit-on-error` into a **disposable PostgreSQL 18 service container that exists only for the job**; the script refuses any non-local target. Schema, migrations, extensions, sequences, indexes, constraints, functions and **every table's exact row count and content hash** are compared with the source snapshot. |
| Encryption | The bundle (dump, both inventories, table of contents, summary, versions) is encrypted with `gpg --symmetric --cipher-algo AES256 --s2k-digest-algo SHA512 --s2k-count 65011712` using the owner's passphrase, then decrypted again in the job to prove it opens. |
| Storage | See section 2. The plaintext dump never leaves the job. |
| Logs | The repository is **public**, so logs print no data: only versions, PASS/FAIL, object counts, sizes and SHA-256 values. Connection strings are GitHub secrets and are masked. |

## 2. Storage destination (private bucket, mandatory)

The production job **requires** a private S3-compatible bucket, such as Cloudflare R2, Backblaze B2 or AWS S3, and fails without one.

* The encrypted archive and its manifest are stored at `champion-db/<archive>.tar.gpg` and `champion-db/<archive>.manifest.txt`.
* **Nothing is uploaded to GitHub**: the repository is public, so no GitHub artifact is used.
* The job then **downloads the archive back from the bucket**, checks its SHA-256, decrypts it, and restores *that retrieved copy* into a second disposable PostgreSQL 18, comparing the full inventory.

A backup counts only when retrieval from the destination and the complete restore both pass. A Railway container's filesystem is never a destination: it is ephemeral and on the same platform as production.

After each run, also keep an owner copy outside the bucket (for example, download it from the bucket console).

## 3. Configuration

**Already configured (2026-09-25).** GitHub environment `production-backup`:

* required reviewer: the owner (`mccartydarion-pixel`);
* **administrators cannot bypass**;
* deployments allowed only from `ops/production-backup`.

**The owner provides** all of the following in that environment (never in the repository; never pasted into chat):

| Kind | Name | Value |
|---|---|---|
| secret | `PROD_DATABASE_URL` | the Railway `Postgres` service's `DATABASE_PUBLIC_URL` |
| secret | `BACKUP_ENCRYPTION_PASSPHRASE` | ≥ 32 random characters, generated in and kept in the owner's password manager |
| variable | `BACKUP_PASSPHRASE_SHA256` | SHA-256 of that passphrase, computed from the password-manager copy (proves the owner retained it; the hash does not reveal it) |
| secret | `BACKUP_S3_ENDPOINT` | e.g. `https://<account-id>.r2.cloudflarestorage.com` |
| secret | `BACKUP_S3_BUCKET` | the private bucket's name |
| secret | `BACKUP_S3_ACCESS_KEY_ID` / `BACKUP_S3_SECRET_ACCESS_KEY` | a key limited to that one bucket (read, write, list) |
| variable (optional) | `BACKUP_S3_REGION` | `auto` (R2, the default), the B2 region such as `us-west-004`, or the AWS region |

**One command instead of the list below:** run `bash ops/backup/setup-secrets.sh` from the repository folder in Git Bash.

* It asks for the R2 account ID, bucket name, access key ID, secret access key and the passphrase (twice) at silent prompts.
* It stores them straight into the environment and records the passphrase fingerprint.
* It copies the database URL from Railway without displaying it, and lists only the configured names.

Then an empty `[backup:connectivity]` commit, approved in GitHub, proves the configuration **without exporting anything**:

* the bucket accepts a write, returns the same bytes, and allows the probe's removal;
* the database accepts a read-only TLS connection;
* the passphrase matches its fingerprint.

**Equivalent individual commands** (run by the owner in Git Bash; values are never displayed):

```
railway variables -s Postgres --kv | grep '^DATABASE_PUBLIC_URL=' | cut -d= -f2- | gh secret set PROD_DATABASE_URL --env production-backup
gh secret set BACKUP_ENCRYPTION_PASSPHRASE --env production-backup      # paste from the password manager
read -rs P && printf '%s' "$P" | sha256sum | cut -d' ' -f1 | gh variable set BACKUP_PASSPHRASE_SHA256 --env production-backup; unset P
gh secret set BACKUP_S3_ENDPOINT --env production-backup
gh secret set BACKUP_S3_BUCKET --env production-backup
gh secret set BACKUP_S3_ACCESS_KEY_ID --env production-backup
gh secret set BACKUP_S3_SECRET_ACCESS_KEY --env production-backup
```

For the fingerprint command, paste the passphrase from the password manager at the silent prompt.

**Optional hardening** (a separate production change): use a dedicated role, for example `champion_backup` with `pg_read_all_data`, in `PROD_DATABASE_URL`, instead of the superuser connection.

## 4. Running a backup

1. The owner gives explicit approval to export production.
2. An **empty** commit is pushed to `ops/production-backup` with `[backup:production]` in the message. A marked commit that changes files is refused, so an ordinary code change can never start an export.
3. GitHub shows the job **waiting for approval** of `production-backup`. The owner approves in the Actions UI.
4. The job:
   1. dumps;
   2. verifies by restore;
   3. encrypts, decrypts to check, and fingerprints;
   4. stores the archive in the private bucket;
   5. downloads it back, checks it, and restores the downloaded copy into a second disposable PostgreSQL 18;
   6. prints the **manifest**: archive name, UTC time, run URL, commit, server and `pg_dump` versions, dump and encrypted sizes and SHA-256 values, and the verification summary.
5. The owner copies the archive and manifest to their own storage.

A run fails, and stores nothing, if any step fails:
* missing secrets, or a passphrase shorter than 32 characters;
* a connection or snapshot error;
* the dump;
* a restore error or **any** inventory difference;
* encryption.

## 5. Verification record (fill in per production run)

| Field | Value |
|---|---|
| Archive | `champion-db-<UTC>-run<id>` |
| Run | GitHub Actions run URL |
| Destination | `s3://<bucket>/champion-db/<archive>.tar.gpg` |
| Retrieval check | downloaded from the bucket, SHA-256 matched, decrypted, restored: PASS |
| Server / pg_dump | 18.6 / 18.6 |
| Dump bytes / SHA-256 | from the manifest |
| Encrypted bytes / SHA-256 | from the manifest |
| Result | `PASS` (identical inventory: migrations, tables, row counts, content hashes) |
| Migrations / last | expected 53 / `0053_installation_embed_activation`, before 0054 is deployed |
| Owner copy | location, and SHA-256 re-checked after download |

## 6. Recovery procedure

Recovery **never runs automatically**. Restoring production is a separate, explicit owner decision.

1. **Get the archive.** Download `champion-db/<archive>.tar.gpg` and its manifest from the private bucket (or use the owner copy). Check `sha256sum <archive>.tar.gpg` against `encrypted_sha256` in the manifest.
2. **Decrypt.** `gpg` ships with Git for Windows:

   ```
   gpg --decrypt -o bundle.tar <archive>.tar.gpg
   tar -xf bundle.tar
   ```

   Enter the passphrase at the prompt. Then check `sha256sum backup.dump` against `dump_sha256`.
3. **Rehearse first.** Restore into a new, empty PostgreSQL 18 database, for example a new Railway Postgres service or a local `postgres:18` container:

   ```
   pg_restore --dbname=<new-db-url> --no-owner --no-privileges --exit-on-error --single-transaction backup.dump
   ```

   Then run `psql <new-db-url> -f ops/backup/inventory.sql | LC_ALL=C sort` and compare it with `source_inventory.txt` from the bundle: the output must be identical.
4. **Production cut-over (owner decision only).** Preferred:
   * point the bot at the verified new database by changing `DATABASE_URL` on the `dayz_killfeed_bot` service. That is a controlled redeploy of the bot only; Champions is not touched.
   * Keep the damaged database untouched for investigation.

   In-place restore over the existing database (`pg_restore --clean --if-exists …`) is a last resort: stop the bot first, and take a fresh backup of the damaged state.

## 7. Change control

* Only the owner's marked push starts an export, and GitHub asks the owner to approve every production run.
* The job has `permissions: contents: read`. It cannot push, merge or deploy.
* Nothing in this branch is merged into `main`.
* The Shop PRs #97, #98 and #95 stay unmerged until a production backup has a `PASS` manifest and the owner has a verified copy.

## 8. Backup log

### 2026-09-26: first production backup (owner-approved), PASS

| Field | Value |
|---|---|
| Archive | `champion-db-20260926T052123Z-run36220469776` |
| Run | https://github.com/mccartydarion-pixel/dayz_killfeed_bot/actions/runs/36220469776 (trigger commit `818af5e`, empty) |
| Destination | private Cloudflare R2 bucket, `champion-db/champion-db-20260926T052123Z-run36220469776.tar.gpg` plus `.manifest.txt` |
| Server / pg_dump | 18.6 / 18.6 |
| Dump | 9,668,660 bytes, SHA-256 `d855c13da84fc814eb864b003568562744860d0b61f5832f1511dc1883d601e9` |
| Encrypted archive | 9,421,604 bytes, SHA-256 `2a54ddb4e7adc6d2ab585522255e2e74aa106fd9b3eac261dcbedc2f1e3f8fe6` (GPG AES256, S2K SHA512 ×65,011,712) |
| Snapshot | one exported REPEATABLE READ, read-only snapshot; `pg_dump` took 25 s |
| Restore 1 (local copy) | archive of 917 entries restored into disposable PostgreSQL 18; inventory identical: **PASS** |
| Restore 2 (retrieved copy) | downloaded from R2, archive SHA-256 matched, decrypted, dump SHA-256 matched, restored into a second disposable PostgreSQL 18; inventory identical: **PASS** |
| Inventory | 53 migrations (last `0053_installation_embed_activation`); 89 tables with exact row counts and content hashes; 425 relations, 275 indexes, 1,071 constraints, 61 sequences, 2 functions, 2 triggers, 1 extension |
| Log check | no connection string or endpoint in the run log |

This is the pre-0054 backup required before PRs #97, #98 and #95 can be deployed.
