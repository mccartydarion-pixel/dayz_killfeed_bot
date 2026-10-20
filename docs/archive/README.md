# Archive

Old plans, one-off investigations and point-in-time reports, kept for history. They describe the
bot, a branch or a pull request **as it was when written** and are not current documentation.
Statements here such as "not merged", "not deployed" or "this PR" are not true today. For current
documentation start at [../README.md](../README.md).

Paths inside these documents are as they were written. Other documents that have since been moved
here are now in this folder.

## Early phase reports

| Document | Why it is archived |
| --- | --- |
| [PHASE_2_REPORT.txt](PHASE_2_REPORT.txt) | Report on an early build phase. |
| [PHASE_4_9_REPORT.txt](PHASE_4_9_REPORT.txt) | Report on an early build phase. |
| [FULL_DIAGNOSTIC_REPORT.txt](FULL_DIAGNOSTIC_REPORT.txt) | A one-off diagnostic of the bot at one point in time. |

## C.A.S.E. (anti-cheat)

| Document | Why it is archived |
| --- | --- |
| [CASE_CORE_FEASIBILITY.md](CASE_CORE_FEASIBILITY.md) | Early feasibility study; says itself that the eight-module list replaced it. |
| [CASE_CORE_BASE_BOOST_FEASIBILITY.md](CASE_CORE_BASE_BOOST_FEASIBILITY.md) | Feasibility study for one detector, written before the base registry and build evidence existed. |
| [CASE_CORE_HITKILL_FEASIBILITY_GATE.md](CASE_CORE_HITKILL_FEASIBILITY_GATE.md) | Note on a source-quality audit for hit and kill lines; a checklist from before the eight-module plan. |
| [CASE_CORE_ADMISSION_POLICY.md](CASE_CORE_ADMISSION_POLICY.md) | Description of a test-only building block (case admission rule) from one pull request. |
| [CASE_CORE_AUDIT_HISTORY_READ.md](CASE_CORE_AUDIT_HISTORY_READ.md) | Pull-request note for a test-only database read; the real route is in `../CASE_CORE_REVIEW_HISTORY_API.md`. |
| [CASE_CORE_AUTHORIZED_REVIEW_READ.md](CASE_CORE_AUTHORIZED_REVIEW_READ.md) | Pull-request note for a test-only database read; the real route is in `../CASE_CORE_REVIEW_QUEUE_API.md`. |
| [CASE_CORE_REVIEW_READ.md](CASE_CORE_REVIEW_READ.md) | Pull-request note for the first review read model; superseded by the review queue API. |
| [CASE_CORE_REVIEW_SCHEMA.md](CASE_CORE_REVIEW_SCHEMA.md) | Proposal for migration 0063, written before it was merged. |
| [CASE_CORE_CURRENT_SOURCE_INSPECTION.md](CASE_CORE_CURRENT_SOURCE_INSPECTION.md) | Pull-request note for a one-off evidence inspection tied to one open question. |
| [CASE_CORE_SOURCE_ACCEPTANCE_WITNESS.md](CASE_CORE_SOURCE_ACCEPTANCE_WITNESS.md) | Pull-request note for a test-only check tied to the same question. |
| [CASE_CORE_EXACT_SHADOW_READBACK.md](CASE_CORE_EXACT_SHADOW_READBACK.md) | Pull-request note for one small database read. |
| [CASE_CORE_OFFLINE_OUTBOX.md](CASE_CORE_OFFLINE_OUTBOX.md) | Says itself it is historical modelling of an alert outbox. |
| [CASE_CORE_OUTBOX_DUE_INSPECTION.md](CASE_CORE_OUTBOX_DUE_INSPECTION.md) | Pull-request note for a test-only outbox diagnostic. |
| [CASE_CORE_OUTBOX_RECONCILIATION.md](CASE_CORE_OUTBOX_RECONCILIATION.md) | Says itself it is a historical offline foundation. |
| [CASE_CORE_SYNTHETIC_OUTBOX_LEASE.md](CASE_CORE_SYNTHETIC_OUTBOX_LEASE.md) | Pull-request note for a test-only outbox claim. |
| [CASE_CORE_SYNTHETIC_REVIEW_TRANSACTION.md](CASE_CORE_SYNTHETIC_REVIEW_TRANSACTION.md) | Says itself it is a historical test-only contract. |
| [CASE_CORE_PRIVATE_DELIVERY_GATE.md](CASE_CORE_PRIVATE_DELIVERY_GATE.md) | Description of an offline rule from before real staff alerts were built (`../CASE_STAFF_ALERTS.md`). |
| [CASE_CORE_OFFLINE_STAFF_PREVIEW.md](CASE_CORE_OFFLINE_STAFF_PREVIEW.md) | Pull-request note for the demo staff card and `cmd/case-staff-preview` (`../TOOLS.md`). |
| [CASE_PHASE2G2.md](CASE_PHASE2G2.md) | Release note for migration 0056, with merge-order instructions for branches that are long merged. |

## Base security

| Document | Why it is archived |
| --- | --- |
| [SECURITY_MARKETPLACE_FOUNDATION.md](SECURITY_MARKETPLACE_FOUNDATION.md) | The first step, when nothing could be bought. Five services are sellable now; each has its own document. |

## Ranked

| Document | Why it is archived |
| --- | --- |
| [RANKED_RP_DESIGN.md](RANKED_RP_DESIGN.md) | The implementation plan for ranked points. Built since: see `../RANKED_SERVER_SEASONS.md` and the other RANKED documents. |
| [RANKED_IDENTITY_AUDIT.md](RANKED_IDENTITY_AUDIT.md) | A one-off code inspection of one pull request. |
| [RANKED_STAGING_SMOKE.md](RANKED_STAGING_SMOKE.md) | The staging test plan for the ranked release. |

## Shop delivery

| Document | Why it is archived |
| --- | --- |
| [SHOP_DELIVERY_PHASE2B.md](SHOP_DELIVERY_PHASE2B.md) | Feasibility research into automatic delivery, before any of it was built. |
| [SHOP_DELIVERY_PHASE2C1.md](SHOP_DELIVERY_PHASE2C1.md) | Report of a read-only look at one server on 2026-09-24. |
| [SHOP_DELIVERY_PHASE2C2.md](SHOP_DELIVERY_PHASE2C2.md) | The review before the first hand-run delivery (the canary). |
| [SHOP_DELIVERY_PHASE2C5.md](SHOP_DELIVERY_PHASE2C5.md) | Integration report for the three canary pull requests. |
| [SHOP_LEDGER_ROLLOUT.md](SHOP_LEDGER_ROLLOUT.md) | The rollout runbook for migrations 0054 and 0055. Both are now in the bot's migration list and run at start-up. |
| [SHOP_GATE_B_CONFIG.md](SHOP_GATE_B_CONFIG.md) | Preparation of a one-time configuration change that has been carried out and whose operation the tool now refuses. |
| [SHOP_CUSTOM_RELOCATION.md](SHOP_CUSTOM_RELOCATION.md) | Preparation of the one-time move of the Shop file into the `custom` folder. |
