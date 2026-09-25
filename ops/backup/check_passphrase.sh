#!/usr/bin/env bash
# Proves, without revealing it, that the encryption passphrase in the environment secret is the one the
# owner retained: the owner records sha256(passphrase) - computed from the password-manager copy - in the
# environment VARIABLE BACKUP_PASSPHRASE_SHA256. A backup is only made when both agree.
# (The hash of a 32+ character random passphrase does not reveal it.)
set -euo pipefail
: "${BACKUP_PASS:?the BACKUP_ENCRYPTION_PASSPHRASE secret is not configured}"
: "${BACKUP_PASSPHRASE_SHA256:?the BACKUP_PASSPHRASE_SHA256 variable (owner-retained passphrase fingerprint) is not configured}"
if [ "${#BACKUP_PASS}" -lt 32 ]; then
  echo "the encryption passphrase must be at least 32 characters" >&2
  exit 1
fi
got="$(printf '%s' "$BACKUP_PASS" | sha256sum | cut -d' ' -f1)"
want="$(printf '%s' "$BACKUP_PASSPHRASE_SHA256" | tr -d '[:space:]' | tr 'A-F' 'a-f')"
if [ "$got" != "$want" ]; then
  echo "the passphrase secret does not match the owner-retained fingerprint: refusing to create an unrecoverable backup" >&2
  exit 1
fi
echo "passphrase: matches the owner-retained fingerprint"
