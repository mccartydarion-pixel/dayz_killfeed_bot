#!/usr/bin/env bash
# One-command owner setup for the production backup (docs/PRODUCTION_BACKUP.md). Run it yourself in Git
# Bash from the repository folder:   bash ops/backup/setup-secrets.sh
#
# It asks for each value at a SILENT prompt (nothing is shown on screen) and stores it directly as a
# secret of the protected GitHub environment `production-backup`. Nothing is written to disk, printed,
# committed or sent anywhere else. The production database URL is copied from Railway without being
# displayed. At the end it lists only the NAMES that are configured.
set -euo pipefail
env_name=production-backup
say() { printf '%s\n' "$*"; }
ask_secret() { # $1 prompt -> REPLY (silent)
  local v=""
  while [ -z "$v" ]; do IFS= read -rs -p "$1: " v; printf '\n'; done
  REPLY="$v"
}

command -v gh >/dev/null || { say "GitHub CLI (gh) is required"; exit 1; }
command -v railway >/dev/null || { say "Railway CLI (railway) is required"; exit 1; }
gh auth status >/dev/null 2>&1 || { say "Run 'gh auth login' first"; exit 1; }
gh api "repos/{owner}/{repo}/environments/${env_name}" >/dev/null 2>&1 || { say "The ${env_name} environment does not exist"; exit 1; }

say "== 1/4 Cloudflare R2 bucket =="
ask_secret "R2 account ID (32 hex characters, from the R2 overview page)"
account="$(printf '%s' "$REPLY" | tr -d '[:space:]')"
[[ "$account" =~ ^[0-9a-fA-F]{32}$ ]] || { say "That does not look like an R2 account ID (32 hex characters)"; exit 1; }
printf '%s' "https://${account}.r2.cloudflarestorage.com" | gh secret set BACKUP_S3_ENDPOINT --env "$env_name"
unset account
ask_secret "Bucket name"
printf '%s' "$REPLY" | tr -d '[:space:]' | gh secret set BACKUP_S3_BUCKET --env "$env_name"
ask_secret "R2 API token: Access Key ID"
printf '%s' "$REPLY" | tr -d '[:space:]' | gh secret set BACKUP_S3_ACCESS_KEY_ID --env "$env_name"
ask_secret "R2 API token: Secret Access Key"
printf '%s' "$REPLY" | tr -d '[:space:]' | gh secret set BACKUP_S3_SECRET_ACCESS_KEY --env "$env_name"
REPLY=""
gh variable set BACKUP_S3_REGION --env "$env_name" --body auto >/dev/null

say "== 2/4 Encryption passphrase =="
say "Generate a passphrase of at least 32 random characters in your password manager and SAVE it there first."
ask_secret "Paste the passphrase"
p1="$REPLY"
ask_secret "Paste it again to confirm"
p2="$REPLY"; REPLY=""
[ "$p1" = "$p2" ] || { say "The two entries differ: nothing stored for the passphrase. Run the script again."; exit 1; }
[ "${#p1}" -ge 32 ] || { say "The passphrase must be at least 32 characters. Nothing stored."; exit 1; }
printf '%s' "$p1" | gh secret set BACKUP_ENCRYPTION_PASSPHRASE --env "$env_name"
printf '%s' "$p1" | sha256sum | cut -d' ' -f1 | gh variable set BACKUP_PASSPHRASE_SHA256 --env "$env_name" >/dev/null
unset p1 p2

say "== 3/4 Production database connection (copied from Railway, not displayed) =="
url="$(railway variables -s Postgres --kv 2>/dev/null | grep '^DATABASE_PUBLIC_URL=' | cut -d= -f2- || true)"
[ -n "$url" ] || { say "Could not read DATABASE_PUBLIC_URL from the Railway Postgres service (is this folder linked with 'railway link'?)"; exit 1; }
printf '%s' "$url" | gh secret set PROD_DATABASE_URL --env "$env_name"
unset url

say "== 4/4 Configured names =="
gh secret list --env "$env_name" | cut -f1
gh variable list --env "$env_name" | cut -f1
say "Done. Tell Claude: 'backup secrets configured'. Keep the passphrase in your password manager - without it no backup can be opened."
