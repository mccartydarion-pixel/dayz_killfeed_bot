# Server Ranked staging smoke test

Scope: isolated QA guild and database, with the existing CHAMPIONS QA application if it can be used without another active gateway process. Never install another bot in the live guild for this test. Use the Nitrado fixture; no real server configuration writes or restarts.

## Preconditions

- Deploy the combined server-only Ranked branch to a separate QA service and disposable PostgreSQL database. Confirm migration 0067 creates only SERVER seasons. Do not point it at the C.A.S.E. staging database or production.
- Use a dedicated QA guild and a single QA bot gateway process. Verify the Discord application, installation, and bot token belong to that QA guild; keep credentials in Railway variables.
- Configure one PlayStation fixture game server and a known server UTC offset learned from the fixture's restart log. Confirm the selected public server belongs to the QA guild.
- Enable the existing `AUTO_LEADERBOARD` and `SERVER_RANKS` routes through /setup in that QA guild. Before a season, expect the V3 five-embed package and one persistent server-ranks placeholder.
- Use test-only rules supplied through the owner season form: 100 RP per eligible kill, cumulative thresholds [100, 300, 600, 1000, 1500, 2100, 2800]. These are QA inputs, not production defaults.

## Run and expected evidence

| Step | Stimulus | Expected result |
|---|---|---|
| 1 | Owner starts the fixture server season | Active SERVER season only; server-ranks placeholder becomes an empty board; V3 Ranks embed appears when it refreshes |
| 2 | Fixture ADM enemy kill at T | One persisted kill and one AWARDED ledger decision (100 RP); regular killfeed still publishes once |
| 3 | Same attacker kills same victim at T+4 minutes | Ordinary killfeed persists; COOLDOWN decision gives 0 RP |
| 4 | Same pair at T+5 minutes | AWARDED decision gives 100 RP; total 200 RP and Rookie tier |
| 5 | Refresh both boards and Player Hub | Same player, 200 RP, Rookie, and the same position; V3 remains one message with six embeds |
| 6 | Replay the ADM lines and restart the QA worker | No duplicate killfeed or RP decisions; existing persistent board messages are edited, not duplicated |
| 7 | Owner resets the local season | Old 200 RP remains archived; new season shows 0 RP and Unranked; all-time kills/streaks/deaths/longest remain unchanged |
| 8 | Remove the learned UTC offset before a new fixture kill, then restore it | No RP while event time is unknown; one decision appears through reconciliation after the offset is learned |

Record only aggregate counts, message IDs, and synthetic fixture IDs. Verify the Discord message count, timestamps, and player-facing page in the QA guild. Stop and investigate any disagreement between `ranked_awards`, the server board, V3, and Player Hub.

## Release gate

Do not merge or deploy the Ranked stack to production on a CI result alone. Complete this smoke test, agree on production RP/threshold values, review the stacked PRs, and obtain the separate production release authorization.
