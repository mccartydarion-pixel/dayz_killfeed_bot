# C.A.S.E. core — eight client modules

Owner-selected replacement scope for the client-facing detector list under #124. The protected detector-readiness read displays these eight names in this order. Every module is **BLOCKED** and the read returns no findings, cases, scores or enforcement. A catalog entry is a product definition, not proof that PlayStation or Xbox exposes its required data.

| Module | Intended observation | Current evidence gap / exit study |
| --- | --- | --- |
| Base Boost Detection | Unauthorized building inside or around another player's base | Need verified ownership, building actions and geometry, with guest/raid/permission exceptions. ADM connect/hit/kill positions cannot establish building. |
| Skywalk Detection | Unexplained movement above valid terrain/structures | Need continuous server-authoritative positions and map/structure geometry, plus vehicle, fall and desync counterexamples. |
| Dupe Detection | Duplicated item creation | Need authoritative item identity and inventory transaction provenance; a reconnect or kill is insufficient. |
| PC Detection (Xbox) | Unapproved PC client participation on an Xbox server | Need trusted platform attestation available to the server. An Xbox account name, behavior or network address does not prove client platform. PlayStation installations should show this as unavailable. |
| No-Clip Detection | Unexplained traversal through collision | Need continuous server-authoritative positions, collision geometry and legitimate transition/lag exclusions. |
| Undermap Detection | Player occupancy below valid map geometry | Need continuous positions and validated terrain/building elevations, with spawn and falling exclusions. |
| Suspicious Logins | Staff-reviewable unusual account/session access | Connect/disconnect lines alone do not establish identity compromise. Need trusted session/device/network context, a baseline and privacy/false-positive review. This is a security review alert, not a cheating verdict. |
| Teleport Alerts | Unexplained location jump | Need trusted game-event elapsed time, continuous position/source coverage and respawn/vehicle/admin/map exceptions. An ADM event pair is not enough. |

The old `CASE-MOV-001` movement-timing prerequisite is an internal, permanently blocked diagnostic used by historical fixture/ledger checks; it is retired from the **client-facing** list. Existing records remain readable, and no detector IDs or evidence histories are rewritten. There is no automatic conversion of an old fixture into a new module.

## Release path

1. Accept current-source retained evidence and protected health/continuity under Gate A.
2. Independently establish a trusted data contract and legitimate counterexamples for one module under Gate B. Unsupported modules remain visibly unavailable; none is promised as active.
3. Only after gates A/B, separately authorize isolated real shadow evaluation and assess false positives. Then gate reviewed cases, private delivery and owner controls in #124 order.

The read-only catalog does not change polling, killfeed, economy, login collection, detection, case admission, Discord alerts or sanctions. Activation and production merge require their respective release approvals.
