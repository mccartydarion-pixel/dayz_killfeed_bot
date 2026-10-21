# C.A.S.E. — private Discord setup category

The shared Channel System V2 planner, used by Discord `/setup run|repair` and the website's auto-setup and repair, adds a separate private category:

- `🔒 CHAMPION • C.A.S.E.`
- `🛡️・case-status` — one readiness information card pointing to the authorized dashboard.
- `📁・case-evidence` — one staff guide card; does not copy player evidence, ADM paths, coordinates or private links.
- `🚨・case-alerts` — one explicitly **NOT ENABLED** notice reserving the destination for a separately reviewed future publisher.

Each installation receives its own scoped `CASE_STATUS`, `CASE_EVIDENCE`, and `CASE_ALERTS` route keys. These are setup-only informational destinations, not live data publishers. A route mapping is **not** a cheating finding or permission to send a synthetic preview as a player accusation. Movement detector `CASE-MOV-001` remains BLOCKED; safe speed pairs are zero, enforcement DISABLED. The existing operational `ADMIN_ALERTS` publisher remains independent.

The category is created with an `@everyone` view deny and explicit bot view/send permissions. Setup never adopts a same-named public category or an existing C.A.S.E. child with an explicit public view override; it creates a private managed destination instead. Re-running setup or repair reuses existing managed channels and does not repost a starter where bot content exists. Existing customer-managed route choices and channels are preserved, and setup never automatically deletes existing channels. A server owner must grant appropriate non-administrator staff roles access to this private category.

The category is created when an installation is connected to the guild and the normal setup permissions and available channel system are satisfied. No bot production deployment or actual Discord channel mutation is performed by this PR. Before any future real C.A.S.E. notifications, separately review detector eligibility, real ADM acceptance, staff authorization, private route permissions, deduplication and evidence privacy.

A customer-manually-selected C.A.S.E. channel is never overwritten. If it is public, setup and the read-only layout status report it as BROKEN rather than treating it as a private staff destination or posting a card into it. Similarly, a category with an explicit public view override is not eligible for automatic reuse even if it also contains an explicit deny. This does not attempt to audit every Discord role permission; staff access still requires guild-owner review.

## Send test alert

`POST .../installations/{installationID}/admin/case/alerts/test` (server owner only; used by the dashboard's **Send test alert** button) posts one sample staff alert card into the installation's `CASE_ALERTS` channel so owners can see what alerts will look like.

- The card is built by the real alert code from made-up sample data ("Sample Player (not real)"). It is marked **TEST** in the message text, the title and the footer. Mentions are disabled.
- It is sent only when the `CASE_ALERTS` route resolves to a channel in the installation's own guild, under a category that denies `@everyone` with no public override on the category or the channel, and the bot can view, send and embed there. Otherwise nothing is sent and the owner sees why.
- It is limited to one test every 30 seconds per installation and audited as `CASE_TEST_ALERT_SENT`.
- It never reads player data or evidence and enables no detector or publisher.
