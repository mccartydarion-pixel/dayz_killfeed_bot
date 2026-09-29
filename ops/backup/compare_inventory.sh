#!/usr/bin/env bash
# Compare the source-snapshot inventory with the restored copy's inventory (docs/PRODUCTION_BACKUP.md).
#
# Every line must match exactly - migrations, objects, triggers, functions, constraints, indexes,
# extensions and every table's row count and content hash - EXCEPT sequence values.
#
# Sequences are not transactional. dump.sh takes the inventory inside the exported snapshot and only
# then runs pg_dump, which records each sequence's value when it reaches it. On a live database the bot
# keeps calling nextval() in between, so the dumped (and restored) value is legitimately equal or
# HIGHER than the inventory's, while the rows those calls inserted are correctly absent from the
# snapshot. A sequence must still exist on both sides with the same name, and a restored value may
# never be LOWER or missing ("unset" in the source accepts any restored value).
#
# Usage: compare_inventory.sh <source_inventory.txt> <target_inventory.txt>
# Prints only difference counts per kind (never names or values). Exit 0 = PASS, 1 = FAIL.
set -euo pipefail
src="${1:?source inventory}"
tgt="${2:?target inventory}"

# Non-negative decimal a >= b, exactly (no floating point): compare length, then digits.
ge() {
  local a=$1 b=$2
  [[ "$a" =~ ^[0-9]+$ && "$b" =~ ^[0-9]+$ ]] || return 1
  a="${a#"${a%%[!0]*}"}"; b="${b#"${b%%[!0]*}"}"; a=${a:-0}; b=${b:-0}
  if (( ${#a} != ${#b} )); then (( ${#a} > ${#b} )); return; fi
  [[ "$a" > "$b" || "$a" == "$b" ]]
}

declare -A kinds=()
# 1. Everything except sequences: exact.
while IFS= read -r line; do
  kind="${line#[<>] }"; kind="${kind%%|*}"; kind="${kind:-blank_line}"
  kinds[$kind]=$(( ${kinds[$kind]:-0} + 1 ))
done < <(diff <(grep -v '^sequence|' "$src" || true) <(grep -v '^sequence|' "$tgt" || true) | grep '^[<>] ' || true)

# 2. Sequences: same names; restored value equal or higher.
declare -A want=() got=()
while IFS='|' read -r _ name value; do want[$name]=$value; done < <(grep '^sequence|' "$src" || true)
while IFS='|' read -r _ name value; do got[$name]=$value; done < <(grep '^sequence|' "$tgt" || true)
for name in "${!want[@]}"; do
  if [[ -z "${got[$name]+x}" ]]; then kinds[sequence_missing]=$(( ${kinds[sequence_missing]:-0} + 1 )); continue; fi
  [[ "${want[$name]}" == unset ]] && continue
  if ! ge "${got[$name]}" "${want[$name]}"; then kinds[sequence_behind]=$(( ${kinds[sequence_behind]:-0} + 1 )); fi
done
for name in "${!got[@]}"; do
  [[ -n "${want[$name]+x}" ]] || kinds[sequence_extra]=$(( ${kinds[sequence_extra]:-0} + 1 ))
done

advanced=0
for name in "${!want[@]}"; do
  [[ -n "${got[$name]+x}" && "${want[$name]}" != unset && "${got[$name]}" != "${want[$name]}" ]] && ge "${got[$name]}" "${want[$name]}" && advanced=$((advanced + 1))
done
echo "sequences_advanced_during_dump=$advanced"

if (( ${#kinds[@]} == 0 )); then
  exit 0
fi
for kind in $(printf '%s\n' "${!kinds[@]}" | LC_ALL=C sort); do
  echo "difference:$kind=${kinds[$kind]}"
done
exit 1
