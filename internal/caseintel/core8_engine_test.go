package caseintel

import (
	"math/rand"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

type flatTerrain float64

func (f flatTerrain) ElevationAt(x, z float64) (float64, bool) {
	if x < 0 || z < 0 || x > 15360 || z > 15360 {
		return 0, false
	}
	return float64(f), true
}

func fresh(static bool) TelemetryFeed {
	return TelemetryFeed{Available: true, Verified: true, Static: static, LatestAt: t0.Add(-10 * time.Second), MaxAge: 2 * time.Minute}
}

// healthyCtx is a synthetic installation with every feed fresh and verified
// and approved fixture thresholds: Strict 1, Balanced 2, Relaxed 3.
func healthyCtx() EvalContext {
	feeds := map[TelemetryKind]TelemetryFeed{}
	for _, k := range []TelemetryKind{TelemetryADMEvents, TelemetryPositionSamples, TelemetryVehicleState, TelemetryBuildActions,
		TelemetryInventoryItems, TelemetryPlatformAttestation, TelemetrySessionEvents} {
		feeds[k] = fresh(false)
	}
	for _, k := range []TelemetryKind{TelemetryTerrainModel, TelemetryStructureGeometry, TelemetryBaseRegistry, TelemetryRestartSchedule} {
		feeds[k] = fresh(true)
	}
	return EvalContext{Scope: Core8Scope{GuildID: 1, InstallationID: 2, ServerID: 3}, PlayerID: 42, PlayerName: "Survivor",
		Enabled: true, Mode: SensitivityBalanced,
		Thresholds: &ValidatedThresholds{Approved: true, MinimumEvidence: 1, Strict: 1, Balanced: 2, Relaxed: 3},
		Telemetry: TelemetrySnapshot{Now: t0, Feeds: feeds, PollingDelay: time.Second, PollingDelayLimit: 30 * time.Second,
			LastSuccessfulEvaluationAt: t0.Add(-time.Minute)},
		Params: DefaultCore8Params()}
}

func alt(v float64) *float64 { return &v }

func timed(id int64, at time.Time) TimedEvidence {
	return TimedEvidence{EvidenceID: id, PlayerID: 42, EventAt: at, TimeTrusted: true, ObservedAt: at.Add(5 * time.Second)}
}

func pos(id int64, sec int, x, z float64) PositionSample {
	return PositionSample{TimedEvidence: timed(id, t0.Add(time.Duration(sec)*time.Second-time.Hour)), X: x, Z: z,
		Altitude: alt(10), VehicleStateKnown: true, LifeID: "life-1"}
}

func assertSafe(t *testing.T, name string, r Core8Result) {
	t.Helper()
	if r.Enforcement != "DISABLED" || r.ViolationEstablished {
		t.Fatalf("%s: enforcement or violation leaked: %+v", name, r)
	}
	// Every catalog module is BLOCKED/UNSUPPORTED, so nothing may notify.
	if r.CanNotify {
		t.Fatalf("%s: unreleased module can notify: %+v", name, r)
	}
	for _, f := range r.Findings {
		if f.Tier == TierStaffConfirmed || f.IncidentKey == "" || len(f.EvidenceIDs) == 0 || f.Scope != (Core8Scope{1, 2, 3}) {
			t.Fatalf("%s: malformed finding %+v", name, f)
		}
	}
}

func teleportPair(base int64, sec int) []PositionSample {
	return []PositionSample{pos(base, sec, 1000, 1000), pos(base+1, sec+10, 3000, 1000)}
}

func TestTeleportLegitimateAndExcludedMovement(t *testing.T) {
	ctx := healthyCtx()
	ctx.Mode = SensitivityStrict
	walk := []PositionSample{pos(1, 0, 1000, 1000), pos(2, 10, 1050, 1000), pos(3, 20, 1100, 1000)}
	if r := EvaluateTeleport(ctx, TeleportInput{Samples: walk}); r.Status != "NO_OBSERVATION" || len(r.Findings) != 0 {
		t.Fatalf("walking flagged: %+v", r)
	}
	cases := []struct {
		name, exclusion string
		mut             func(s []PositionSample, in *TeleportInput)
	}{
		{"gap", "SAMPLE_GAP_TOO_LARGE", func(s []PositionSample, _ *TeleportInput) {
			s[1].EventAt = s[0].EventAt.Add(10 * time.Minute)
			s[1].ObservedAt = s[1].EventAt.Add(time.Second)
		}},
		{"respawn", "RESPAWN_OR_LIFE_UNKNOWN", func(s []PositionSample, _ *TeleportInput) { s[1].LifeID = "life-2" }},
		{"vehicle", "VEHICLE_MOVEMENT", func(s []PositionSample, _ *TeleportInput) { s[0].InVehicle = true }},
		{"vehicle unknown", "VEHICLE_STATE_UNKNOWN", func(s []PositionSample, _ *TeleportInput) { s[1].VehicleStateKnown = false }},
		{"untrusted clock", "EVENT_TIME_UNTRUSTED", func(s []PositionSample, _ *TeleportInput) { s[1].TimeTrusted = false }},
		{"stale sample", "SAMPLE_STALE", func(s []PositionSample, _ *TeleportInput) { s[1].ObservedAt = s[1].EventAt.Add(time.Hour) }},
		{"other player", "PLAYER_MISMATCH", func(s []PositionSample, _ *TeleportInput) { s[1].PlayerID = 7 }},
		{"restart", "SERVER_RESTART", func(s []PositionSample, in *TeleportInput) {
			in.Restarts = []RestartWindow{{Start: s[0].EventAt.Add(2 * time.Second), End: s[0].EventAt.Add(4 * time.Second)}}
		}},
		{"exempt zone", "TELEPORT_EXEMPT_ZONE", func(s []PositionSample, in *TeleportInput) {
			in.Map.Zones = []Zone{{Kind: "TELEPORT_EXEMPT", X: 3000, Z: 1000, Radius: 50}}
		}},
	}
	for _, tc := range cases {
		s := teleportPair(10, 0)
		in := TeleportInput{}
		tc.mut(s, &in)
		in.Samples = s
		r := EvaluateTeleport(ctx, in)
		assertSafe(t, tc.name, r)
		if len(r.Findings) != 0 || r.Exclusions[tc.exclusion] == 0 {
			t.Fatalf("%s: want exclusion %s, got %+v", tc.name, tc.exclusion, r)
		}
	}
	r := EvaluateTeleport(ctx, TeleportInput{Samples: teleportPair(10, 0)})
	assertSafe(t, "jump", r)
	if r.Status != "REVIEW_CANDIDATE" || len(r.Findings) != 1 || r.Findings[0].Tier != TierSuspicious ||
		r.Findings[0].EvidenceCompleteness != "COMPLETE" || len(r.Findings[0].Coordinates) != 2 {
		t.Fatalf("jump not surfaced: %+v", r)
	}
}

func TestSensitivityChangesRepetitionNotEvidence(t *testing.T) {
	samples := append(teleportPair(10, 0), teleportPair(20, 60)...)
	for _, tc := range []struct {
		mode Sensitivity
		want string
	}{{SensitivityStrict, "REVIEW_CANDIDATE"}, {SensitivityBalanced, "REVIEW_CANDIDATE"}, {SensitivityRelaxed, "OBSERVATION_ONLY"}} {
		ctx := healthyCtx()
		ctx.Mode = tc.mode
		r := EvaluateTeleport(ctx, TeleportInput{Samples: samples})
		assertSafe(t, string(tc.mode), r)
		if r.Status != tc.want || len(r.Findings) != 2 {
			t.Fatalf("%s: %+v", tc.mode, r)
		}
		if tc.want == "OBSERVATION_ONLY" && r.Findings[0].Tier != TierObserved {
			t.Fatalf("relaxed labelled suspicious: %+v", r.Findings[0])
		}
	}
	// Strict cannot bypass evidence: an untrusted sample stays excluded.
	ctx := healthyCtx()
	ctx.Mode = SensitivityStrict
	bad := teleportPair(10, 0)
	bad[1].TimeTrusted = false
	if r := EvaluateTeleport(ctx, TeleportInput{Samples: bad}); len(r.Findings) != 0 {
		t.Fatalf("strict bypassed evidence: %+v", r)
	}
	// Unapproved thresholds never produce a candidate.
	ctx.Thresholds = nil
	r := EvaluateTeleport(ctx, TeleportInput{Samples: samples})
	if r.Status != "OBSERVATION_ONLY" || !hasString(r.Reasons, "THRESHOLD_NOT_VALIDATED") {
		t.Fatalf("unapproved thresholds: %+v", r)
	}
}

func TestIdempotentAcrossRetriesAndIsolatedAcrossInstallations(t *testing.T) {
	ctx := healthyCtx()
	samples := append(teleportPair(10, 0), teleportPair(20, 60)...)
	first := EvaluateTeleport(ctx, TeleportInput{Samples: samples})
	// Replay: shuffled order plus duplicate rows from a polling retry.
	replay := append(append([]PositionSample(nil), samples...), samples[0], samples[3])
	rand.New(rand.NewSource(7)).Shuffle(len(replay), func(i, j int) { replay[i], replay[j] = replay[j], replay[i] })
	second := EvaluateTeleport(ctx, TeleportInput{Samples: replay})
	if len(first.Findings) != 2 || len(second.Findings) != 2 || second.Exclusions["DUPLICATE_SAMPLE"] != 2 {
		t.Fatalf("replay changed result: %+v / %+v", first, second)
	}
	for i := range first.Findings {
		if first.Findings[i].IncidentKey != second.Findings[i].IncidentKey {
			t.Fatalf("incident key not stable")
		}
	}
	other := healthyCtx()
	other.Scope.InstallationID = 99
	iso := EvaluateTeleport(other, TeleportInput{Samples: samples})
	if iso.Findings[0].IncidentKey == first.Findings[0].IncidentKey {
		t.Fatalf("incident key shared across installations")
	}
	if _, err := IncidentKey(ctx.Scope, "CASE-TELEPORT-001", "0.1.0", 42, []int64{1, 1}); err == nil {
		t.Fatalf("duplicate evidence accepted")
	}
}

func TestTelemetryHealthSuspendsConclusions(t *testing.T) {
	jump := TeleportInput{Samples: teleportPair(10, 0)}
	cases := []struct {
		name, status string
		mut          func(*EvalContext)
	}{
		{"disabled", "DISABLED", func(c *EvalContext) { c.Enabled = false }},
		{"stale positions", "SUSPENDED", func(c *EvalContext) {
			f := c.Telemetry.Feeds[TelemetryPositionSamples]
			f.LatestAt = t0.Add(-time.Hour)
			c.Telemetry.Feeds[TelemetryPositionSamples] = f
		}},
		{"stale vehicle feed", "SUSPENDED", func(c *EvalContext) {
			f := c.Telemetry.Feeds[TelemetryVehicleState]
			f.LatestAt = t0.Add(-time.Hour)
			c.Telemetry.Feeds[TelemetryVehicleState] = f
		}},
		{"polling behind", "SUSPENDED", func(c *EvalContext) { c.Telemetry.PollingDelay = time.Hour }},
		{"missing telemetry", "INSUFFICIENT_EVIDENCE", func(c *EvalContext) { delete(c.Telemetry.Feeds, TelemetryPositionSamples) }},
		{"unverified feed", "INSUFFICIENT_EVIDENCE", func(c *EvalContext) {
			f := c.Telemetry.Feeds[TelemetryVehicleState]
			f.Verified = false
			c.Telemetry.Feeds[TelemetryVehicleState] = f
		}},
		{"unsupported feed", "UNSUPPORTED", func(c *EvalContext) {
			c.Telemetry.Feeds[TelemetryVehicleState] = TelemetryFeed{Unsupported: true}
		}},
		{"processing error", "ERROR", func(c *EvalContext) { c.Telemetry.ProcessingError = true }},
		{"bad scope", "ERROR", func(c *EvalContext) { c.Scope.ServerID = 0 }},
	}
	for _, tc := range cases {
		ctx := healthyCtx()
		tc.mut(&ctx)
		r := EvaluateTeleport(ctx, jump)
		if r.Status != tc.status || len(r.Findings) != 0 || r.CanNotify || len(r.Reasons) == 0 {
			t.Fatalf("%s: %+v", tc.name, r)
		}
	}
	h := AssessCore8Health("CASE-TELEPORT-001", true, healthyCtx().Telemetry)
	if h.State != "INSUFFICIENT_EVIDENCE" || !h.ConclusionsSuspended || !hasString(h.Reasons, "MODULE_NOT_RELEASED") {
		t.Fatalf("blocked catalog module reported healthy: %+v", h)
	}
	for _, d := range ClientCatalog() {
		if len(RequiredTelemetry(d.ID)) == 0 {
			t.Fatalf("%s has no telemetry contract", d.ID)
		}
		if got := AssessCore8Health(d.ID, true, healthyCtx().Telemetry); got.State == "ACTIVE" {
			t.Fatalf("%s became ACTIVE", d.ID)
		}
	}
	if len(ClientCatalog()) != 8 || len(requiredTelemetry) != 8 {
		t.Fatalf("Core Eight scope changed")
	}
}

func elevated(id int64, sec int, a float64) PositionSample {
	s := pos(id, sec, 5000, 5000)
	s.Altitude = alt(a)
	return s
}

func TestSkywalkAndUndermap(t *testing.T) {
	ctx := healthyCtx()
	ctx.Mode = SensitivityStrict
	m := MapModel{Name: "chernarusplus", Verified: true, Terrain: flatTerrain(100)}
	run := []PositionSample{elevated(1, 0, 140), elevated(2, 6, 141), elevated(3, 12, 142)}
	r := EvaluateSkywalk(ctx, ElevationInput{Samples: run, Map: m})
	assertSafe(t, "skywalk", r)
	if r.Status != "REVIEW_CANDIDATE" || len(r.Findings) != 1 || len(r.Findings[0].EvidenceIDs) != 3 {
		t.Fatalf("skywalk run: %+v", r)
	}
	// The next poll extends the same run: still one incident, same key.
	extended := EvaluateSkywalk(ctx, ElevationInput{Samples: append(append([]PositionSample(nil), run...), elevated(4, 18, 143)), Map: m})
	if len(extended.Findings) != 1 || extended.Findings[0].IncidentKey != r.Findings[0].IncidentKey || len(extended.Findings[0].EvidenceIDs) != 4 {
		t.Fatalf("extended run opened a new incident: %+v", extended)
	}
	if r := EvaluateSkywalk(ctx, ElevationInput{Samples: run[:1], Map: m}); len(r.Findings) != 0 || r.Exclusions["NOT_REPEATED"] != 1 {
		t.Fatalf("single sample flagged: %+v", r)
	}
	tower := m
	tower.Zones = []Zone{{Kind: "STRUCTURE", Label: "watchtower", X: 5000, Z: 5000, Radius: 10}}
	if r := EvaluateSkywalk(ctx, ElevationInput{Samples: run, Map: tower}); len(r.Findings) != 0 || r.Exclusions["STRUCTURE_ZONE"] != 3 {
		t.Fatalf("structure not excluded: %+v", r)
	}
	heli := append([]PositionSample(nil), run...)
	heli[1].InVehicle = true
	if r := EvaluateSkywalk(ctx, ElevationInput{Samples: heli, Map: m}); len(r.Findings) != 0 {
		t.Fatalf("vehicle altitude flagged: %+v", r)
	}
	if r := EvaluateSkywalk(ctx, ElevationInput{Samples: run, Map: MapModel{Name: "custom", Terrain: flatTerrain(0)}}); len(r.Findings) != 0 || !hasString(r.Reasons, "MAP_MODEL_UNVERIFIED") {
		t.Fatalf("unverified custom map evaluated: %+v", r)
	}

	under := []PositionSample{elevated(1, 0, 90), elevated(2, 6, 89), elevated(3, 12, 88)}
	r = EvaluateUndermap(ctx, ElevationInput{Samples: under, Map: m})
	assertSafe(t, "undermap", r)
	if len(r.Findings) != 1 {
		t.Fatalf("undermap run: %+v", r)
	}
	bunker := m
	bunker.Zones = []Zone{{Kind: "UNDERGROUND", Label: "bunker", X: 5000, Z: 5000, Radius: 30, MaxAlt: alt(99)}}
	if r := EvaluateUndermap(ctx, ElevationInput{Samples: under, Map: bunker}); len(r.Findings) != 0 || r.Exclusions["UNDERGROUND_ZONE"] != 3 {
		t.Fatalf("bunker not excluded: %+v", r)
	}
	if r := EvaluateUndermap(ctx, ElevationInput{Samples: []PositionSample{elevated(1, 0, 99), elevated(2, 6, 98)}, Map: m}); len(r.Findings) != 0 {
		t.Fatalf("shallow terrain noise flagged: %+v", r)
	}
}

func TestNoClip(t *testing.T) {
	ctx := healthyCtx()
	ctx.Mode = SensitivityStrict
	m := MapModel{Name: "chernarusplus", Verified: true, Solids: []Solid{{Label: "wall", MinX: 99, MaxX: 101, MinZ: 0, MaxZ: 200, MinAlt: 0, MaxAlt: 20}}}
	through := []PositionSample{pos(1, 0, 98, 100), pos(2, 1, 102, 100)}
	r := EvaluateNoClip(ctx, NoClipInput{Samples: through, Map: m})
	assertSafe(t, "noclip", r)
	if len(r.Findings) != 1 {
		t.Fatalf("wall crossing missed: %+v", r)
	}
	door := m
	door.Zones = []Zone{{Kind: "ENTRANCE", X: 100, Z: 100, Radius: 2}}
	if r := EvaluateNoClip(ctx, NoClipInput{Samples: through, Map: door}); len(r.Findings) != 0 || r.Exclusions["LEGITIMATE_ENTRANCE"] != 1 {
		t.Fatalf("entrance not excluded: %+v", r)
	}
	sparse := []PositionSample{pos(1, 0, 98, 100), pos(2, 30, 102, 100)}
	if r := EvaluateNoClip(ctx, NoClipInput{Samples: sparse, Map: m}); len(r.Findings) != 0 || r.Exclusions["SPARSE_SAMPLES"] != 1 {
		t.Fatalf("sparse snapshots established no-clip: %+v", r)
	}
	over := []PositionSample{pos(1, 0, 98, 100), pos(2, 1, 102, 100)}
	over[0].Altitude, over[1].Altitude = alt(25), alt(25)
	if r := EvaluateNoClip(ctx, NoClipInput{Samples: over, Map: m}); len(r.Findings) != 0 {
		t.Fatalf("climbing over wall flagged: %+v", r)
	}
	if r := EvaluateNoClip(ctx, NoClipInput{Samples: through, Map: MapModel{Verified: true}}); !hasString(r.Reasons, "COLLISION_GEOMETRY_UNVERIFIED") || len(r.Findings) != 0 {
		t.Fatalf("missing geometry: %+v", r)
	}
}

func TestBaseBoost(t *testing.T) {
	ctx := healthyCtx()
	ctx.Mode = SensitivityStrict
	faction := int64(9)
	base := RegisteredBase{BaseID: "base-1", Verified: true, OwnerPlayerID: 100, OwnerFactionID: &faction,
		X: 2000, Z: 2000, Radius: 50, RegisteredAt: t0.Add(-48 * time.Hour), AuthorizedPlayers: []int64{55}}
	other := int64(3)
	build := func(id int64) BuildAction {
		return BuildAction{TimedEvidence: timed(id, t0.Add(-time.Hour)), Action: "built", Object: "Watchtower", X: 2010, Z: 2010, FactionID: &other}
	}
	r := EvaluateBaseBoost(ctx, BaseBoostInput{Actions: []BuildAction{build(1)}, Bases: []RegisteredBase{base}})
	assertSafe(t, "boost", r)
	if len(r.Findings) != 1 || r.Findings[0].AffectedBaseID != "base-1" || r.Findings[0].Behavior == "" {
		t.Fatalf("unauthorized build missed: %+v", r)
	}
	cases := []struct {
		name, exclusion string
		mut             func(*BuildAction, *RegisteredBase)
	}{
		{"owner", "AUTHORIZED_PLAYER", func(a *BuildAction, b *RegisteredBase) { b.OwnerPlayerID = 42 }},
		{"guest", "AUTHORIZED_PLAYER", func(a *BuildAction, b *RegisteredBase) { b.AuthorizedPlayers = []int64{42} }},
		{"faction", "AUTHORIZED_FACTION", func(a *BuildAction, b *RegisteredBase) { a.FactionID = &faction }},
		{"faction unknown", "FACTION_MEMBERSHIP_UNKNOWN", func(a *BuildAction, b *RegisteredBase) { a.FactionID = nil }},
		{"before registration", "NOT_REGISTERED_AT_EVENT_TIME", func(a *BuildAction, b *RegisteredBase) { b.RegisteredAt = t0 }},
		{"unverified base", "BASE_UNVERIFIED", func(a *BuildAction, b *RegisteredBase) { b.Verified = false }},
		{"untrusted time", "EVENT_TIME_UNTRUSTED", func(a *BuildAction, b *RegisteredBase) { a.TimeTrusted = false }},
	}
	for _, tc := range cases {
		a, b := build(1), base
		tc.mut(&a, &b)
		r := EvaluateBaseBoost(ctx, BaseBoostInput{Actions: []BuildAction{a}, Bases: []RegisteredBase{b}})
		if len(r.Findings) != 0 || r.Exclusions[tc.exclusion] == 0 {
			t.Fatalf("%s: %+v", tc.name, r)
		}
	}
	far := build(1)
	far.X = 3000
	if r := EvaluateBaseBoost(ctx, BaseBoostInput{Actions: []BuildAction{far}, Bases: []RegisteredBase{base}}); len(r.Findings) != 0 {
		t.Fatalf("build outside radius flagged: %+v", r)
	}
}

func TestDupeRequiresItemEvidence(t *testing.T) {
	ctx := healthyCtx()
	ctx.Mode = SensitivityStrict
	var sessions []SessionEvent
	for i := 0; i < 10; i++ {
		sessions = append(sessions, SessionEvent{TimedEvidence: timed(int64(100+i), t0.Add(time.Duration(i-60)*time.Minute)), Kind: []string{"CONNECT", "DISCONNECT"}[i%2]})
	}
	r := EvaluateDupe(ctx, DupeInput{Sessions: sessions})
	assertSafe(t, "no items", r)
	if len(r.Findings) != 0 || !hasString(r.Reasons, "ITEM_EVIDENCE_UNAVAILABLE") {
		t.Fatalf("reconnects alone produced dupe observation: %+v", r)
	}
	item := func(id int64, min int, kind string) ItemEvent {
		return ItemEvent{TimedEvidence: timed(id, t0.Add(time.Duration(min-60)*time.Minute)), ItemID: "M4A1#123", ItemType: "M4A1", Kind: kind, ProvenanceVerified: true}
	}
	dup := []ItemEvent{item(1, 1, "ACQUIRED"), item(2, 3, "ACQUIRED")}
	r = EvaluateDupe(ctx, DupeInput{Sessions: sessions, Items: dup})
	assertSafe(t, "dupe", r)
	if len(r.Findings) != 1 || r.Findings[0].Tier != TierSuspicious {
		t.Fatalf("item-level dupe missed: %+v", r)
	}
	released := []ItemEvent{item(1, 1, "ACQUIRED"), item(2, 2, "RELEASED"), item(3, 3, "ACQUIRED")}
	if r := EvaluateDupe(ctx, DupeInput{Sessions: sessions, Items: released}); len(r.Findings) != 0 {
		t.Fatalf("legitimate transfer flagged: %+v", r)
	}
	if r := EvaluateDupe(ctx, DupeInput{Items: dup}); len(r.Findings) != 0 || r.Exclusions["NO_RECONNECT_OR_RESTART_CONTEXT"] != 1 {
		t.Fatalf("no reconnect context: %+v", r)
	}
	unverified := []ItemEvent{item(1, 1, "ACQUIRED"), item(2, 3, "ACQUIRED")}
	unverified[1].ProvenanceVerified = false
	if r := EvaluateDupe(ctx, DupeInput{Sessions: sessions, Items: unverified}); len(r.Findings) != 0 {
		t.Fatalf("unverified provenance used: %+v", r)
	}
}

func TestPCXboxRequiresTrustedAttestation(t *testing.T) {
	ctx := healthyCtx()
	ctx.Mode = SensitivityStrict
	att := PlatformAttestation{TimedEvidence: timed(1, t0.Add(-time.Minute)), Platform: "PC", Source: "trusted-attestor", Trusted: true, SignatureVerified: true}
	if r := EvaluatePCXbox(ctx, PCDetectionInput{InstallationPlatform: "PS5", Attestations: []PlatformAttestation{att}}); r.Status != "UNSUPPORTED" || len(r.Findings) != 0 {
		t.Fatalf("non-Xbox: %+v", r)
	}
	if r := EvaluatePCXbox(ctx, PCDetectionInput{InstallationPlatform: "XBOX"}); r.Status != "UNSUPPORTED" || !hasString(r.Reasons, "TRUSTED_PLATFORM_ATTESTATION_UNAVAILABLE") {
		t.Fatalf("no attestation: %+v", r)
	}
	untrusted := att
	untrusted.SignatureVerified = false
	if r := EvaluatePCXbox(ctx, PCDetectionInput{InstallationPlatform: "XBOX", Attestations: []PlatformAttestation{untrusted}}); r.Status != "UNSUPPORTED" || len(r.Findings) != 0 {
		t.Fatalf("untrusted attestation used: %+v", r)
	}
	xbox := att
	xbox.Platform = "XBOX"
	if r := EvaluatePCXbox(ctx, PCDetectionInput{InstallationPlatform: "XBOX", Attestations: []PlatformAttestation{xbox}}); len(r.Findings) != 0 || r.Status != "NO_OBSERVATION" {
		t.Fatalf("xbox client flagged: %+v", r)
	}
	r := EvaluatePCXbox(ctx, PCDetectionInput{InstallationPlatform: "XBOX", Attestations: []PlatformAttestation{att}})
	assertSafe(t, "pc", r)
	if len(r.Findings) != 1 {
		t.Fatalf("attested PC missed: %+v", r)
	}
	// Production reality: the ADM collector cannot attest platforms.
	ctx.Telemetry.Feeds[TelemetryPlatformAttestation] = TelemetryFeed{Unsupported: true}
	if r := EvaluatePCXbox(ctx, PCDetectionInput{InstallationPlatform: "XBOX", Attestations: []PlatformAttestation{att}}); r.Status != "UNSUPPORTED" || len(r.Findings) != 0 {
		t.Fatalf("unsupported feed evaluated: %+v", r)
	}
}

func TestSuspiciousLogins(t *testing.T) {
	ctx := healthyCtx()
	ctx.Mode = SensitivityStrict
	var s []SessionEvent
	id := int64(1)
	add := func(sec int, kind string) {
		s = append(s, SessionEvent{TimedEvidence: timed(id, t0.Add(time.Duration(sec)*time.Second-time.Hour)), Kind: kind})
		id++
	}
	add(0, "CONNECT")
	add(600, "DISCONNECT")
	add(660, "CONNECT") // one ordinary reconnect
	if r := EvaluateSuspiciousLogins(ctx, LoginInput{Sessions: s}); len(r.Findings) != 0 {
		t.Fatalf("ordinary reconnect flagged: %+v", r)
	}
	for i := 0; i < 4; i++ {
		add(700+i*60, "DISCONNECT")
		add(720+i*60, "CONNECT")
	}
	r := EvaluateSuspiciousLogins(ctx, LoginInput{Sessions: s, RelatedIncidentKeys: []string{"abc"}})
	assertSafe(t, "burst", r)
	if len(r.Findings) != 1 || len(r.Findings[0].RelatedIncidentKeys) != 1 {
		t.Fatalf("burst missed: %+v", r)
	}
	add(3000, "CONNECT") // connect->connect long after: a missing disconnect line
	if got := EvaluateSuspiciousLogins(ctx, LoginInput{Sessions: s}); len(got.Findings) != 1 || got.Findings[0].IncidentKey != r.Findings[0].IncidentKey {
		t.Fatalf("missing disconnect line changed the burst: %+v", got)
	}
	restart := []RestartWindow{{Start: t0.Add(-time.Hour + 650*time.Second), End: t0.Add(-time.Hour + 700*time.Second)}}
	if r := EvaluateSuspiciousLogins(ctx, LoginInput{Sessions: s, Restarts: restart}); len(r.Findings) != 0 || r.Exclusions["SERVER_RESTART"] == 0 {
		t.Fatalf("restart reconnects flagged: %+v", r)
	}
}

func TestStaffConfirmationIsExplicit(t *testing.T) {
	ctx := healthyCtx()
	ctx.Mode = SensitivityStrict
	r := EvaluateTeleport(ctx, TeleportInput{Samples: teleportPair(10, 0)})
	f := r.Findings[0]
	if _, err := ConfirmByStaff(f, " "); err == nil {
		t.Fatalf("confirmed without reviewer")
	}
	c, err := ConfirmByStaff(f, "staff:1")
	if err != nil || c.Tier != TierStaffConfirmed || f.Tier != TierSuspicious {
		t.Fatalf("confirm: %v %+v", err, c)
	}
	if _, err := ConfirmByStaff(c, "staff:2"); err == nil {
		t.Fatalf("double confirmation accepted")
	}
	obs := f
	obs.Tier = TierObserved
	if _, err := ConfirmByStaff(obs, "staff:1"); err == nil {
		t.Fatalf("observation confirmed directly")
	}
}
