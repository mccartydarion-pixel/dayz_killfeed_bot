# C.A.S.E. — synthetic staff card preview

This branch is stacked on Phase 2G.5 draft PR #113 and addresses issue #115. Its only renderer is `discord.BuildCASEStaffDemoEmbed()`, a fixed Go fixture. It takes **no player, source, server, finding, or credential input**, creates a fresh embed on demand, and uses a fixed fixture timestamp. There is no publisher wiring, route, new endpoint, command, scheduler, webhook, or send call in this change. It does not create a C.A.S.E. case or risk score.

The card says **SYNTHETIC PREVIEW — NOT A REAL PLAYER OR DETECTION** and reports the only defensible current detector state: `CASE-MOV-001 BLOCKED`, safe speed pairs zero and enforcement disabled. No accusation, player identity, private location, or false source-coverage claim is shown.

This is not a live staff notification and should not be sent to the live Champions Discord as a cheating alert. Existing `ADMIN_ALERTS` operational alerts do not become C.A.S.E. cheating findings. Real finding-based notifications need independent detector validation, authorization, routing, deduplication, privacy and review. Real Champions source coverage remains unverified under issue #114. Do not merge this branch ahead of PRs #112 and #113 or bypass their release gates.
