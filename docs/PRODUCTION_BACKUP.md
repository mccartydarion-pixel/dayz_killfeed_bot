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

## 2. Storage destination

| Option | When | Properties |
|---|---|---|
| **Private S3-compatible bucket** (Cloudflare R2, Backblaze B2, AWS S3, …) | Used automatically when `BACKUP_S3_*` secrets exist | Private, durable, retention set by the bucket. The GitHub artifact is **then skipped**. **Recommended.** |
| Encrypted GitHub Actions artifact | Fallback when no bucket is configured | Stored by GitHub outside Railway, **90 days**. On a public repository any signed-in GitHub user can download the file, but it is AES-256 ciphertext, protected only by the passphrase. Copy it to owner-controlled storage and delete the artifact afterwards. |
| Owner copy | Always, after each run | `gh run download <run-id> -n <archive> -D <folder>`. Keep it in the owner's own storage, next to the manifest. |

A Railway container's filesystem is **not** a backup destination: it is ephemeral and lives on the same platform as production.

## 3. What the owner must provide (once)

1. **A GitHub environment** (Settings → Environments → New environment): name `production-backup`.
   * Required reviewers: the owner.
   * Deployment branches: **Selected branches → `ops/production-backup`**.

   Create it **before** adding the secrets, and add the secrets to this environment, not to the repository.
2. **Environment secrets**:
   * `PROD_DATABASE_URL`: the Railway `Postgres` service's `DATABASE_PUBLIC_URL`. From a machine with the Railway CLI linked to the project, without displaying it:

     ```
     railway variables -s Postgres --kv | grep '^DATABASE_PUBLIC_URL=' | cut -d= -f2- | gh secret set PROD_DATABASE_URL --env production-backup
     ```
   * `BACKUP_ENCRYPTION_PASSPHRASE`: at least 32 random characters. Generate it in your password manager and store it there. **Without it the backup cannot be opened; nobody can recover it.** Then run `gh secret set BACKUP_ENCRYPTION_PASSPHRASE --env production-backup` and paste it at the prompt.
3. **Optional, recommended**: a private bucket, with `BACKUP_S3_ENDPOINT`, `BACKUP_S3_BUCKET`, `BACKUP_S3_ACCESS_KEY_ID` and `BACKUP_S3_SECRET_ACCESS_KEY` as environment secrets. Use a key limited to writing and listing that one bucket.

   For Cloudflare R2 the endpoint is `https://<account-id>.r2.cloudflarestorage.com`; Backblaze B2 gives an S3 endpoint per region.
4. **Optional hardening** (a separate production change, needs approval): a dedicated backup role instead of the owner connection, for example `CREATE ROLE champion_backup LOGIN PASSWORD … ; GRANT pg_read_all_data TO champion_backup;`. `PROD_DATABASE_URL` would then use that role.

## 4. Running a backup

1. The owner gives explicit approval to export production.
2. An empty commit is pushed to `ops/production-backup` with `[backup:production]` in the message.
3. GitHub shows the job **waiting for approval** of `production-backup`. The owner approves in the Actions UI.
4. The job:
   1. dumps;
   2. verifies by restore;
   3. encrypts, decrypts to check, and fingerprints;
   4. stores the archive;
   5. prints the **manifest**: archive name, UTC time, run URL, commit, server and `pg_dump` versions, dump and encrypted sizes and SHA-256 values, and the verification summary.
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
| Destination | bucket path, or artifact name and expiry |
| Server / pg_dump | 18.6 / 18.6 |
| Dump bytes / SHA-256 | from the manifest |
| Encrypted bytes / SHA-256 | from the manifest |
| Result | `PASS` (identical inventory: migrations, tables, row counts, content hashes) |
| Migrations / last | expected 53 / `0053_installation_embed_activation`, before 0054 is deployed |
| Owner copy | location, and SHA-256 re-checked after download |

## 6. Recovery procedure

Recovery **never runs automatically**. Restoring production is a separate, explicit owner decision.

1. **Get the archive.** Download it from the bucket or with `gh run download <run-id> -n <archive>`. Check `sha256sum <archive>.tar.gpg` against `encrypted_sha256` in the manifest.
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
