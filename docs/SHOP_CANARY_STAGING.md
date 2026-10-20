# Champion Shop canary — staging (Gate E) and unstaging (Gate G)

This is the file side of the single-item canary. Gates E and G write only `custom/champion_shop_delivery.json`, and only for **one ledger attempt**:
* the payload is derived from the attempt's immutable ledger facts (attempt ID, class, quantity, position, artifact path) with `nitradodelivery.SingleAttemptFiles`;
* the tool reads those facts from the ledger over a read-only connection;
* nobody supplies the payload.

The tool never writes the database. It prints the verified read-back hashes, which the operator records through the canary operator API (`/shop/canary/attempts`). The API re-checks them against the same facts (`ARTIFACT_HASH_MISMATCH`).

Each run is a separate owner approval, uses a separate plan ID, and follows a dry run.

## Preconditions (all verified live, bound into the plan ID)

| | Gate E `gate-e-stage` | Gate G `gate-g-unstage` |
|---|---|---|
| Attempt state (ledger) | `FILE_PREPARED` | `FILE_STAGED` or `AWAITING_RESTART` (abort before any start), or `UNSTAGE_REQUIRED` (after the restart) |
| Attempt artifact | `custom/champion_shop_delivery.json`; a real Champion attempt ID (never delivery 0) | same |
| Current Champion file | exactly the 20-byte empty file (`328c4d64…`); anything else, including another attempt, is refused | exactly **this** attempt's staged file |
| Payload | this attempt's staged file | the empty file |
| Configuration | the expected SHA-256; `objectSpawnersArr` references `custom/champion_shop_delivery.json` | same |

The general machinery of Gates A–D also applies:
* the plan ID is single-use and journaled before the write;
* one token, one transfer, no retries, https `*.nitrado.net` only;
* the outcome is decided by read-back;
* `UNCERTAIN` blocks further writes;
* after the write: the config is unchanged and no restart occurred.

## Sequence (owner-present window)

1. **Before the purchase:**
   * **Ledger:** migration 0068 is live (done 2026-09-30), and this code is deployed, so new attempts record `custom/…`.
   * **Canary product:** 1 point, stock 1, limit 1.
   * **Drop point:** fresh from the current boot, with the owner standing there.
2. **Canary lock:** open it for installation 11. That's `CHAMPION_SHOP_CANARY_EXECUTION=enabled` plus `CHAMPION_SHOP_CANARY_INSTALLATION_IDS=11`, which redeploys the bot only.
3. **Purchase:** the owner buys the canary through the Shop. The operator then creates the attempt (`PLAN_CREATED`) and advances it to `FILE_PREPARED`.
4. **Gate E:**
   * dry run: `-operation gate-e-stage -attempt <id> …`;
   * execute with the owner's approval of that plan ID;
   * record `STAGED_FILE_HASH` (with the previous hash) and `STAGING_BOOT`, then advance to `FILE_STAGED` and `AWAITING_RESTART`.
5. **Restart** (owner-triggered, or the next scheduled one). Record `NEW_BOOT`, then advance to `RESTART_OBSERVED` and `UNSTAGE_REQUIRED`.
6. **Gate G, immediately:**
   * dry run: `-operation gate-g-unstage -attempt <id> …`;
   * execute;
   * record `UNSTAGED_FILE_HASH`, then advance to `VERIFICATION_REQUIRED`.
7. **In game:** the named observer records `ITEM_OBSERVED` and `PICKUP_CONFIRMED`.
8. **Second restart:** record `SECOND_BOOT`. In game, record `NO_ADDITIONAL_SPAWN`. Then fulfil.
9. **Close the lock:** unset both variables.

**Aborting before any restart:** from `FILE_STAGED` or `AWAITING_RESTART`, Gate G restores the empty file, and the attempt moves to `UNSTAGED`. A refund then becomes possible.

## Commands

The tool needs `NITRADO_TOKEN` (bot service) **and** `DATABASE_PUBLIC_URL` (Postgres service, used read-only):

```
railway run -s Postgres -- bash -c 'railway run -s dayz_killfeed_bot -- env DATABASE_PUBLIC_URL="$DATABASE_PUBLIC_URL" \
  go run ./cmd/shop-mission-write -operation gate-e-stage -attempt <attempt id> \
  -service 19806451 -org 1 -installation 11 -game-server 1 \
  -mission dayzps_missions/dayzOffline.chernarusplus -path custom/champion_shop_delivery.json \
  -expect-current <empty sha> -expect-config-sha256 <config sha> \
  -expect-spawners '"'"'["custom/The_Lost_City.json","custom/champion_shop_delivery.json"]'"'"' \
  -expect-payload-sha256 <staged sha>'
```

The dry run prints the attempt, the staged and empty hashes, the plan and its ID. Add `-execute -authorize <plan ID>` only after approval.
