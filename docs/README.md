# Champion bot documentation

This is the index of everything in `docs/`. Each line says what the document is for.

How to read these documents:

- Most were written when a feature was built, so many carry a phase number in the title
  ("Phase 1", "Phase 4"). The phase is history; the content describes what is in the code today
  unless the document says otherwise. Some documents still use the wording of the change that
  introduced them ("this PR", "draft", "candidate"); where that could mislead, a note at the top
  says what is true now.
- "Off by default" means the code is in the bot but does nothing until a switch is turned on. The
  switches are environment variables (see [`.env.example`](../.env.example)) or owner settings.
- [`archive/`](archive/README.md) holds finished plans, one-off investigations and point-in-time
  reports. Nothing in the archive describes the bot as it is now.
- For what the bot is and how to run and test it, see the [repository README](../README.md).

## Getting started and operations

| Document | What it is for |
| --- | --- |
| [../README.md](../README.md) | What the bot is, how to run it, how to run the tests. |
| [../.env.example](../.env.example) | Every environment variable the bot reads, with defaults. |
| [TOOLS.md](TOOLS.md) | The helper programs in `cmd/`: what each does, what it can touch, whether it is still needed. |
| [PERFORMANCE.md](PERFORMANCE.md) | Audit of the event pipeline's speed, what was tuned and the tuning variables. |
| [NITRADO_POLLING.md](NITRADO_POLLING.md) | How often server logs are read, Nitrado rate limits and the adaptive polling speed. |
| [NITRADO_DELTA_READS.md](NITRADO_DELTA_READS.md) | Partial log reads (`NITRADO_DELTA_READ_MODE`, off by default) and how to probe a server first. |
| [CHAMPION_LIVE_SYNC.md](CHAMPION_LIVE_SYNC.md) | How log changes are detected, checkpointed and recorded, and how the right log file is chosen after a restart. |
| [runtime-status-api.md](runtime-status-api.md) | `GET /api/runtime/status`: live runtime state for the website, including the deploy self-check. |
| [DEPLOY.md](DEPLOY.md) | How a release goes live: where the deploy time goes (measured), the Railway settings, start-up and readiness, the deploy self-check and shutdown. |
| [SERVER_STATUS.md](SERVER_STATUS.md) | `GET .../admin/server-status`: "is my killfeed working" for a server owner. The route, who may call it and the exact JSON contract. |
| [incidents/](#incident-records-september-2026) | Dated records of the September 2026 reliability incident and its release. Historical. |

## Discord features

| Document | What it is for |
| --- | --- |
| [DISCORD_PRESENTATION_V2.md](DISCORD_PRESENTATION_V2.md) | The visual design of every default Discord card. |
| [SAAS_RUNTIME_ROUTING.md](SAAS_RUNTIME_ROUTING.md) | How each feature finds its Discord channel (channel routes), with notes per feed. |
| [ADMIN_LOGS.md](ADMIN_LOGS.md) | The private staff channel: log monitor, admin alerts and build feed. |
| [AUTO_LEADERBOARD_V3.md](AUTO_LEADERBOARD_V3.md) | The persistent leaderboard message. |
| [BOUNTY_SYSTEM.md](BOUNTY_SYSTEM.md) | Bounties: placing, claiming, the board and the tracking feed. |
| [DEATH_COUNTS.md](DEATH_COUNTS.md) | Exactly what counts as a death in every statistic. |
| [ONLINE_COUNTER_AND_LINK_CHECK.md](ONLINE_COUNTER_AND_LINK_CHECK.md) | The online-player counter, player presence and gamertag linking rules. |
| [MULTI_PROCESS.md](MULTI_PROCESS.md) | What happens when two bot processes run at once: which workers run only on the leader and what is still unsafe. |
| [SERVER_NAME_SYNC.md](SERVER_NAME_SYNC.md) | How a server's displayed name follows Nitrado unless the owner set one. |
| [FEATURES_CHANNEL.md](FEATURES_CHANNEL.md) | The `/features` channel that lists new features for players. |
| [FEED_IDENTITY.md](FEED_IDENTITY.md) | Posting feeds under a server's own name and avatar. |
| [EMBED_DESIGNER_V2.md](EMBED_DESIGNER_V2.md) | Embed Designer: preview and test send of a custom card. |
| [EMBED_RUNTIME.md](EMBED_RUNTIME.md) | Using saved custom cards in the live feeds (`CHAMPION_CUSTOM_EMBEDS_ENABLED`, off by default). |
| [CHAMPION_CARD.md](CHAMPION_CARD.md) | The shareable picture of a player's stats. |
| [LIVES.md](LIVES.md) | Lives: the stretch between two deaths, and what is shown about it. |
| [FIGHT_REPLAY.md](FIGHT_REPLAY.md) | Kills grouped into fights, replayable on the map. |
| [HOT_ZONES.md](HOT_ZONES.md) | Automatic short events where a fight is already happening. |
| [HEATMAPS.md](HEATMAPS.md) | Kill, death, activity and intrusion heatmaps, and the Discord heatmap picture. |
| [ZONES_UAV_RADAR.md](ZONES_UAV_RADAR.md) | Map zones and intrusion alerts (UAV, Base Radar). |
| [RANKED_SERVER_SEASONS.md](RANKED_SERVER_SEASONS.md) | Starting and resetting a server's ranked season. |
| [RANKED_PLAYER_PROGRESS.md](RANKED_PLAYER_PROGRESS.md) | A player's ranked progress in the Player Hub. |
| [RANKED_DOUBLE_RP.md](RANKED_DOUBLE_RP.md) | Double RP windows. |
| [RANKED_BONUSES.md](RANKED_BONUSES.md) | Optional ranked bonuses. |
| [PROGRESSION.md](PROGRESSION.md) | Challenges, battle pass and territory. |
| [FEATURE_UPGRADES.md](FEATURE_UPGRADES.md) | Per-server automations layered on existing features. |
| [NETWORK.md](NETWORK.md) | The public cross-server directory and leaderboards. |

## Website API (SaaS)

| Document | What it is for |
| --- | --- |
| [SAAS_API.md](SAAS_API.md) | **The main contract** between the website and the bot: authentication, routes, examples. Start here. |
| [SAAS_HTTP_API.md](SAAS_HTTP_API.md) | Supplementary implementation notes for the same API. |
| [saas-openapi.yaml](saas-openapi.yaml) | Machine-readable version of the customer API. Incomplete: newer routes are documented only in the feature documents on this page. |
| [PLAYER_API.md](PLAYER_API.md) | Player Hub: which servers a player has history on and their stats. |
| [CLIENT_ADMIN.md](CLIENT_ADMIN.md) | Server-admin permissions, the audit log and admin actions. |
| [CLIENT_HUB_GROWTH.md](CLIENT_HUB_GROWTH.md) | Nine Client Hub tools for owners and staff (planner, events, seasons and more). |
| [PLAYER_INTELLIGENCE.md](PLAYER_INTELLIGENCE.md) | Player directory and location history. |
| [RETENTION.md](RETENTION.md) | Who is playing, who came back, who stopped. |
| [FACTIONS.md](FACTIONS.md) | Faction Hub: directory, membership, recruitment, logos. |
| [FACTION_STATS.md](FACTION_STATS.md) | Faction statistics, achievements and activity. |
| [FACTION_LEADERBOARDS.md](FACTION_LEADERBOARDS.md) | Faction leaderboards. |
| [ECONOMY.md](ECONOMY.md) | Champion Points on the website: balances, history, admin grants. |
| [ECONOMY_SYSTEM.md](ECONOMY_SYSTEM.md) | The Champion Points ledger underneath. |
| [PERK_STORE.md](PERK_STORE.md) | The perk store (Donate tab): perks sold for Champion Points. |

## Owner Hub API (platform admin)

| Document | What it is for |
| --- | --- |
| [ADMIN_API.md](ADMIN_API.md) | The cross-customer API behind the Owner Hub: roles, routes, owner controls, feature flags. |
| [OWNER_OPS.md](OWNER_OPS.md) | Owner Hub operations pages: fleet, customer health, revenue, broadcasts, self-healing monitor. |
| [admin-openapi.yaml](admin-openapi.yaml) | Machine-readable version of the first, read-only admin routes. Incomplete: see ADMIN_API.md and OWNER_OPS.md for the rest. |

## Live map

| Document | What it is for |
| --- | --- |
| [LIVE_MAP.md](LIVE_MAP.md) | The website's live map: public, faction and staff views and what each may see. |

## Map rotation

| Document | What it is for |
| --- | --- |
| [MAP_ROTATION.md](MAP_ROTATION.md) | Switching a server's map on restart, with a player vote. Off until three switches are on. |

## Stadium

| Document | What it is for |
| --- | --- |
| [STADIUM.md](STADIUM.md) | The tournament arena: the layout, the owner API, the guarded write to the server and the manual install steps. |

## Shop

| Document | What it is for |
| --- | --- |
| [SHOP.md](SHOP.md) | The Champion Shop: products, purchases with Champion Points, refunds. |
| [SHOP_DELIVERY.md](SHOP_DELIVERY.md) | Delivery records and delivery by staff, with or without map coordinates. |
| [SHOP_ORDER_CONFIRMATION.md](SHOP_ORDER_CONFIRMATION.md) | The buyer's "received / issue" answer and support tickets. |
| [SHOP_DELIVERY_WORKER.md](SHOP_DELIVERY_WORKER.md) | Automatic in-game delivery as built: its switches and what to do when it stops. Off by default. |
| [SHOP_DELIVERY_WORKER_DESIGN.md](SHOP_DELIVERY_WORKER_DESIGN.md) | The design behind the automatic delivery worker and the owner's decisions. |
| [SHOP_DELIVERY_PHASE2C3.md](SHOP_DELIVERY_PHASE2C3.md) | The delivery attempt ledger (database rules that keep a delivery from being lost or doubled). |
| [SHOP_DELIVERY_PHASE2C4.md](SHOP_DELIVERY_PHASE2C4.md) | The canary operator API: recording a hand-run delivery step by step. Locked by default. |
| [SHOP_GATE_A_UPLOAD.md](SHOP_GATE_A_UPLOAD.md) | The guarded Nitrado file write behind `cmd/shop-mission-write`: protocol, journal, exit codes. |
| [SHOP_CANARY_STAGING.md](SHOP_CANARY_STAGING.md) | Staging and unstaging one item by hand with `cmd/shop-mission-write`. |

## C.A.S.E. (anti-cheat) and base security

No C.A.S.E. detector is active: all eight are blocked in the code. What runs today is evidence
collection (off by default), read-only views for staff, and the base services players can buy.

| Document | What it is for |
| --- | --- |
| [CASE_SETUP.md](CASE_SETUP.md) | The owner's setup checklist on the Anti-cheat page. |
| [CASE_DISCORD_SETUP.md](CASE_DISCORD_SETUP.md) | The private C.A.S.E. Discord category that `/setup` creates. |
| [CASE_CORE_EIGHT_MODULES.md](CASE_CORE_EIGHT_MODULES.md) | The eight detectors and what evidence each still lacks. |
| [CASE_CORE8_DETECTION_ENGINE.md](CASE_CORE8_DETECTION_ENGINE.md) | The detection engine: code map, stages, shadow reads, release gate. |
| [CASE_CORE_ADVANCED_ROADMAP.md](CASE_CORE_ADVANCED_ROADMAP.md) | The approved roadmap for the eight detectors. A plan, not a status report. |
| [CASE_STAFF_ALERTS.md](CASE_STAFF_ALERTS.md) | When a real staff alert would be sent, and how a detector is released. Nothing is sent today. |
| [CASE_LOGIN_SHADOW_REVIEW.md](CASE_LOGIN_SHADOW_REVIEW.md) | Runbook: reviewing the Suspicious Logins detector on real traffic before release. |
| [CASE_BASE_SHADOW_REVIEW.md](CASE_BASE_SHADOW_REVIEW.md) | Runbook: reviewing the Base Boosting detector before release. |
| [CASE_PHASE2.md](CASE_PHASE2.md) | The observation overview API. |
| [CASE_PHASE2B.md](CASE_PHASE2B.md) | Evidence collection (`CASE_EVIDENCE_ENABLED`, off by default). |
| [CASE_PHASE2C.md](CASE_PHASE2C.md) | Player session reconstruction (read-only). |
| [CASE_PHASE2D.md](CASE_PHASE2D.md) | Evidence quality and detector readiness checks. |
| [CASE_PHASE2E.md](CASE_PHASE2E.md) | The source integrity view. |
| [CASE_PHASE2E1.md](CASE_PHASE2E1.md) | How the right log file is recovered after a server restart. |
| [CASE_PHASE2F.md](CASE_PHASE2F.md) | Evidence continuity reporting. |
| [CASE_PHASE2G1.md](CASE_PHASE2G1.md) | The detector registry and the detector-readiness API. |
| [CASE_PHASE2G3.md](CASE_PHASE2G3.md) | Shadow diagnostics and the shadow-history API. |
| [CASE_PHASE2G5.md](CASE_PHASE2G5.md) | Evidence admissibility: what the console log can and cannot prove. |
| [CASE_CORE_REVIEW_QUEUE_API.md](CASE_CORE_REVIEW_QUEUE_API.md) | The review queue API (read-only; empty until a detector is released). |
| [CASE_CORE_REVIEW_HISTORY_API.md](CASE_CORE_REVIEW_HISTORY_API.md) | The review history API for one case (read-only). |
| [CASE_EMBED_DESIGNER_CONTRACT.md](CASE_EMBED_DESIGNER_CONTRACT.md) | C.A.S.E. cards in the Embed Designer: stored and previewable, not sent. |
| [CASE_DISCORD_DELIVERY_RECONCILIATION.md](CASE_DISCORD_DELIVERY_RECONCILIATION.md) | Watch digest delivery to Discord, what "unknown" delivery means, and the QA probe. |
| [BASE_REQUESTS.md](BASE_REQUESTS.md) | Players asking for their base to be registered. |
| [BASE_TRANSFERS.md](BASE_TRANSFERS.md) | Handing a base to a faction mate. |
| [BASE_RENT.md](BASE_RENT.md) | Rent, in Champion Points, for player-requested bases. |
| [BASE_RAID_ALARM.md](BASE_RAID_ALARM.md) | A DM to the owner when their base is being taken apart. |
| [PERIMETER_WATCH.md](PERIMETER_WATCH.md) | A DM to the owner when someone comes near their base. |
| [BASE_BLACK_BOX.md](BASE_BLACK_BOX.md) | A history of who came near a base or took parts off it. |
| [FACTION_SECURITY.md](FACTION_SECURITY.md) | Sharing base alerts with the owner's faction. |
| [SENTINEL_PRO.md](SENTINEL_PRO.md) | The bundle that covers every base service. |
| [SECURITY_GIFTS.md](SECURITY_GIFTS.md) | The owner giving a player free time on a base service. |
| [SECURITY_SALES.md](SECURITY_SALES.md) | The owner's sales summary for the Security Store. |
| [SECURITY_STORE_PANEL.md](SECURITY_STORE_PANEL.md) | The Security Store message in Discord. |

## Billing

| Document | What it is for |
| --- | --- |
| [BILLING.md](BILLING.md) | Stripe subscriptions: checkout, portal, webhooks, trials, the plan catalog, open decisions. |
| [CASE_PHASE6_BILLING.md](CASE_PHASE6_BILLING.md) | The paid C.A.S.E. add-on: how it is billed and gated. Built, switched off by default. |
| [CASE_STRIPE_TESTMODE_QA.md](CASE_STRIPE_TESTMODE_QA.md) | Checklist for testing the C.A.S.E. add-on in Stripe test mode before any sale. |

## Database and schema

| Document | What it is for |
| --- | --- |
| [SAAS_SCHEMA.md](SAAS_SCHEMA.md) | The tables behind the website API, and the rule that the bot owns the schema. |

Migrations themselves are in `internal/database/migrations.go`; they run when the bot starts.
Most feature documents name the migration that added their tables.

## Internal design notes

Background for developers. These explain why something is built the way it is.

| Document | What it is for |
| --- | --- |
| [PERFORMANCE.md](PERFORMANCE.md) | What was measured and why each tuning choice was made. |
| [CHAMPION_LIVE_SYNC.md](CHAMPION_LIVE_SYNC.md) | Audit and design of the log pipeline. |
| [NITRADO_DELTA_READS.md](NITRADO_DELTA_READS.md) | Design record for partial log reads. |
| [SAAS_RUNTIME_ROUTING.md](SAAS_RUNTIME_ROUTING.md) | Audit of how feeds were moved onto channel routes. |
| [SHOP_DELIVERY_WORKER_DESIGN.md](SHOP_DELIVERY_WORKER_DESIGN.md) | Design of automatic Shop delivery. |
| [CASE_CORE_ADVANCED_ROADMAP.md](CASE_CORE_ADVANCED_ROADMAP.md) | The C.A.S.E. roadmap. |

## Incident records (September 2026)

Written during the 2026-09-26 reliability incident and the release that followed. They record
what was known on the day; several say "not deployed" or "not run" because they were written
before the release. Kept in place because the code refers to them.

| Document | What it is for |
| --- | --- |
| [incidents/2026-09-26-P0-reliability.md](incidents/2026-09-26-P0-reliability.md) | The incident and its recovery. |
| [incidents/2026-09-26-consolidation.md](incidents/2026-09-26-consolidation.md) | How the three fixes were combined into one release candidate. |
| [incidents/2026-09-26-immediate-killfeed-validation.md](incidents/2026-09-26-immediate-killfeed-validation.md) | Validation of immediate killfeed delivery. |
| [incidents/2026-09-26-staging-infrastructure.md](incidents/2026-09-26-staging-infrastructure.md) | The plan for an isolated staging environment (the Nitrado fixture). |
| [incidents/2026-09-26-staging-preflight.md](incidents/2026-09-26-staging-preflight.md) | Access and deployment checks before staging. |
| [incidents/2026-09-26-qa-reuse-audit.md](incidents/2026-09-26-qa-reuse-audit.md) | Whether existing QA services could be reused for staging. |
| [incidents/2026-09-26-phase3-staging-deployment.md](incidents/2026-09-26-phase3-staging-deployment.md) | The staging deployment approval report. |
| [incidents/2026-09-26-staging-verification.md](incidents/2026-09-26-staging-verification.md) | The staging verification report. |
| [incidents/2026-09-26-production-release-runbook.md](incidents/2026-09-26-production-release-runbook.md) | The production release checklist. |
| [incidents/2026-09-26-evidence-queries.sql](incidents/2026-09-26-evidence-queries.sql) | Read-only database queries used as incident evidence. |
| [incidents/2026-09-26-release-preflight-queries.sql](incidents/2026-09-26-release-preflight-queries.sql) | Read-only database queries run before and after the release. |

## Archive

[archive/README.md](archive/README.md) lists every archived document and why it was archived.
