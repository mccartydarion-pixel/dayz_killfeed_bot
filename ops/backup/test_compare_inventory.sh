#!/usr/bin/env bash
# Offline regression tests for compare_inventory.sh (no database, no network).
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
t="$(mktemp -d)"
trap 'rm -rf "$t"' EXIT
base() {
  cat <<'EOF'
constraint|public.kills|kills_pkey|0f343b0931126a20f133d67c2b018a3b
index|public.kills_pkey|5d41402abc4b2a76b9719d911017c592
migration|0001_init
migration|0066_discord_feed_cards
sequence|public.feed_cards_id_seq|200
sequence|public.kills_id_seq|500
table|public.kills|500|7d793037a0760186574b0282f2f435e7
EOF
}
pass=0
check() { # name expected(PASS|FAIL) expected-output-substring source target
  local name=$1 want=$2 needle=$3 got out
  printf '%s\n' "$4" | LC_ALL=C sort > "$t/s"; printf '%s\n' "$5" | LC_ALL=C sort > "$t/t"
  if out=$(bash "$here/compare_inventory.sh" "$t/s" "$t/t"); then got=PASS; else got=FAIL; fi
  if [[ "$got" != "$want" || "$out" != *"$needle"* ]]; then
    echo "FAIL: $name: got $got, output: $out" >&2; exit 1
  fi
  if grep -qE 'public\.|[0-9a-f]{32}|\|' <<<"$out"; then echo "FAIL: $name: output leaks names or values: $out" >&2; exit 1; fi
  pass=$((pass + 1))
}
B="$(base)"
check "identical"                     PASS "sequences_advanced_during_dump=0" "$B" "$B"
L="${B/feed_cards_id_seq|200/feed_cards_id_seq|214}"
check "live nextval during pg_dump"   PASS "sequences_advanced_during_dump=2" "$B" "${L/kills_id_seq|500/kills_id_seq|531}"
check "stray blank line"              FAIL "difference:blank_line=1"           "$B" "$B"$'\n'$'\n'"migration|0001_init"
check "restored sequence lower"       FAIL "difference:sequence_behind=1"      "$B" "${B/kills_id_seq|500/kills_id_seq|499}"
check "restored sequence missing"     FAIL "difference:sequence_missing=1"     "$B" "$(grep -v kills_id_seq <<<"$B")"
check "unexpected extra sequence"     FAIL "difference:sequence_extra=1"       "$B" "$B"$'\n'"sequence|public.other_seq|1"
check "non-numeric restored value"    FAIL "difference:sequence_behind=1"      "$B" "${B/kills_id_seq|500/kills_id_seq|unset}"
check "unset source accepts any"      PASS "sequences_advanced_during_dump=0" "${B/kills_id_seq|500/kills_id_seq|unset}" "$B"
check "beyond 2^53, exact (ahead)"    PASS "" "${B/kills_id_seq|500/kills_id_seq|9007199254740992}" "${B/kills_id_seq|500/kills_id_seq|9007199254740993}"
check "beyond 2^53, exact (behind)"   FAIL "difference:sequence_behind=1" "${B/kills_id_seq|500/kills_id_seq|9007199254740993}" "${B/kills_id_seq|500/kills_id_seq|9007199254740992}"
check "table row count differs"       FAIL "difference:table=2"               "$B" "${B/kills|500|/kills|499|}"
check "table content hash differs"    FAIL "difference:table=2"               "$B" "${B/7d793037a0760186574b0282f2f435e7/00000000000000000000000000000000}"
check "migration missing"             FAIL "difference:migration=1"           "$B" "$(grep -v 0066_ <<<"$B")"
check "constraint definition differs" FAIL "difference:constraint=2"          "$B" "${B/0f343b0931126a20f133d67c2b018a3b/ffffffffffffffffffffffffffffffff}"
echo "compare_inventory tests: $pass passed"
