package caseintel

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// PositionSample is one server-authoritative position with its real event
// time. VehicleStateKnown=false means a vehicle cannot be ruled out, so any
// check that needs to exclude vehicles skips the sample.
type PositionSample struct {
	TimedEvidence
	X, Z              float64
	Altitude          *float64
	InVehicle         bool
	VehicleStateKnown bool
	LifeID            string // changes on every recorded death/respawn
}

// TerrainModel returns ground elevation for a verified map. ok=false means
// the point is outside the model and no elevation conclusion is possible.
type TerrainModel interface {
	ElevationAt(x, z float64) (elevation float64, ok bool)
}

// Zone is a circular exclusion with an optional altitude band. Kinds used:
// STRUCTURE (towers, rooftops, custom builds), UNDERGROUND (bunkers, tunnels),
// ENTRANCE (doors, gaps, legitimate passages), TELEPORT_EXEMPT (admin or
// scripted teleports, spawn areas).
type Zone struct {
	Kind           string
	Label          string
	X, Z, Radius   float64
	MinAlt, MaxAlt *float64
}

func (z Zone) contains(x, zz float64, alt *float64) bool {
	if dist2D(z.X, z.Z, x, zz) > z.Radius {
		return false
	}
	if alt == nil {
		return z.MinAlt == nil && z.MaxAlt == nil
	}
	return (z.MinAlt == nil || *alt >= *z.MinAlt) && (z.MaxAlt == nil || *alt <= *z.MaxAlt)
}

// Solid is an axis-aligned collision volume (wall, rock, sealed building).
type Solid struct {
	Label                  string
	MinX, MaxX, MinZ, MaxZ float64
	MinAlt, MaxAlt         float64
}

// MapModel is the per-installation map contract, including custom maps and
// owner-registered custom structures. Verified must reflect an independent
// review of the terrain and geometry for this exact map revision.
type MapModel struct {
	Name     string
	Verified bool
	Terrain  TerrainModel
	Zones    []Zone
	Solids   []Solid
}

func (m MapModel) inZone(kind string, x, z float64, alt *float64) (string, bool) {
	for _, zn := range m.Zones {
		if zn.Kind == kind && zn.contains(x, z, alt) {
			return zn.Label, true
		}
	}
	return "", false
}

// orderedSamples validates timestamps and returns usable samples ordered by
// trusted event time; ties keep evidence order for determinism.
func orderedSamples(ev *evaluation, samples []PositionSample) []PositionSample {
	out := make([]PositionSample, 0, len(samples))
	seen := map[int64]bool{}
	for _, s := range samples {
		if p := s.sampleProblem(ev.ctx); p != "" {
			ev.exclude(p)
			continue
		}
		if seen[s.EvidenceID] {
			ev.exclude("DUPLICATE_SAMPLE")
			continue
		}
		seen[s.EvidenceID] = true
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].EventAt.Equal(out[j].EventAt) {
			return out[i].EventAt.Before(out[j].EventAt)
		}
		return out[i].EvidenceID < out[j].EvidenceID
	})
	return out
}

// TeleportInput covers one player's window. Consecutive trusted samples are
// compared; a gap is unobserved movement and is never itself evidence.
type TeleportInput struct {
	Samples  []PositionSample
	Restarts []RestartWindow
	Map      MapModel
}

func EvaluateTeleport(ctx EvalContext, in TeleportInput) Core8Result {
	ev, blocked := newEvaluation(ctx, "CASE-TELEPORT-001")
	if blocked != nil {
		return *blocked
	}
	p := ctx.Params
	s := orderedSamples(ev, in.Samples)
	for i := 1; i < len(s); i++ {
		a, b := s[i-1], s[i]
		dt := b.EventAt.Sub(a.EventAt)
		switch {
		case dt <= 0:
			ev.exclude("NON_MONOTONIC_TIME")
			continue
		case p.TeleportMaxGap <= 0 || dt > p.TeleportMaxGap:
			ev.exclude("SAMPLE_GAP_TOO_LARGE")
			continue
		case a.LifeID == "" || a.LifeID != b.LifeID:
			ev.exclude("RESPAWN_OR_LIFE_UNKNOWN")
			continue
		case spansRestart(in.Restarts, p.RestartGrace, a.EventAt, b.EventAt):
			ev.exclude("SERVER_RESTART")
			continue
		case !a.VehicleStateKnown || !b.VehicleStateKnown:
			ev.exclude("VEHICLE_STATE_UNKNOWN")
			continue
		case a.InVehicle || b.InVehicle:
			ev.exclude("VEHICLE_MOVEMENT")
			continue
		}
		if _, ok := in.Map.inZone("TELEPORT_EXEMPT", a.X, a.Z, nil); ok {
			ev.exclude("TELEPORT_EXEMPT_ZONE")
			continue
		}
		if _, ok := in.Map.inZone("TELEPORT_EXEMPT", b.X, b.Z, nil); ok {
			ev.exclude("TELEPORT_EXEMPT_ZONE")
			continue
		}
		d := dist2D(a.X, a.Z, b.X, b.Z)
		speed := d / dt.Seconds()
		if p.TeleportMaxFootSpeed <= 0 || d < p.TeleportMinDistance || speed <= p.TeleportMaxFootSpeed {
			continue
		}
		ev.observe(Finding{EvidenceIDs: []int64{a.EvidenceID, b.EvidenceID}, EventAt: b.EventAt, ObservedAt: b.ObservedAt,
			Coordinates: []Coordinates{coords(a.X, a.Z, a.Altitude), coords(b.X, b.Z, b.Altitude)},
			Behavior:    "Position change faster than on-foot movement",
			Explanation: fmt.Sprintf("%.0f m in %s (%.1f m/s) between two trusted samples in the same life, on foot, outside restart and exempt zones; limit %.1f m/s.",
				d, dt.Round(time.Second), speed, p.TeleportMaxFootSpeed)},
			"RESPAWN", "SERVER_RESTART", "VEHICLE", "TELEPORT_EXEMPT_ZONE", "SAMPLE_GAP")
	}
	return ev.finalize()
}

// ElevationInput is shared by Skywalk and Undermap.
type ElevationInput struct {
	Samples []PositionSample
	Map     MapModel
}

func EvaluateSkywalk(ctx EvalContext, in ElevationInput) Core8Result {
	return evaluateElevation(ctx, in, "CASE-SKYWALK-001", true)
}

func EvaluateUndermap(ctx EvalContext, in ElevationInput) Core8Result {
	return evaluateElevation(ctx, in, "CASE-UNDERMAP-001", false)
}

// evaluateElevation groups consecutive out-of-terrain samples within one life
// into runs; only a run meeting the repeat count and duration becomes an
// observation, so one bad sample or a fall cannot produce an incident.
func evaluateElevation(ctx EvalContext, in ElevationInput, moduleID string, above bool) Core8Result {
	ev, blocked := newEvaluation(ctx, moduleID)
	if blocked != nil {
		return *blocked
	}
	if !in.Map.Verified || in.Map.Terrain == nil {
		ev.reasons = append(ev.reasons, "MAP_MODEL_UNVERIFIED")
		return ev.finalize()
	}
	p := ctx.Params
	var run []PositionSample
	var offsets []float64
	flush := func() {
		defer func() { run, offsets = nil, nil }()
		if len(run) == 0 {
			return
		}
		if p.ElevationMinConsecutive <= 1 || len(run) < p.ElevationMinConsecutive ||
			run[len(run)-1].EventAt.Sub(run[0].EventAt) < p.ElevationMinDuration {
			ev.exclude("NOT_REPEATED")
			return
		}
		f := Finding{EventAt: run[0].EventAt, ObservedAt: run[len(run)-1].ObservedAt}
		minOff := math.Inf(1)
		for i, s := range run {
			f.EvidenceIDs = append(f.EvidenceIDs, s.EvidenceID)
			f.Coordinates = append(f.Coordinates, coords(s.X, s.Z, s.Altitude))
			minOff = math.Min(minOff, offsets[i])
		}
		f.keyEvidenceIDs = append([]int64(nil), f.EvidenceIDs[:p.ElevationMinConsecutive]...)
		span := run[len(run)-1].EventAt.Sub(run[0].EventAt).Round(time.Second)
		if above {
			f.Behavior = "Sustained position above terrain with no registered structure"
			f.Explanation = fmt.Sprintf("%d consecutive trusted samples over %s at least %.1f m above verified terrain on %s, outside registered structures and not in a vehicle.", len(run), span, minOff, in.Map.Name)
			ev.observe(f, "STRUCTURE_ZONE", "VEHICLE", "RESPAWN", "SINGLE_SAMPLE")
		} else {
			f.Behavior = "Sustained position below terrain outside underground areas"
			f.Explanation = fmt.Sprintf("%d consecutive trusted samples over %s at least %.1f m below verified terrain on %s, outside registered bunkers and tunnels.", len(run), span, minOff, in.Map.Name)
			ev.observe(f, "UNDERGROUND_ZONE", "STRUCTURE_ZONE", "RESPAWN", "SINGLE_SAMPLE")
		}
	}
	for _, s := range orderedSamples(ev, in.Samples) {
		if len(run) > 0 && (s.LifeID == "" || s.LifeID != run[len(run)-1].LifeID) {
			flush()
		}
		if s.Altitude == nil {
			ev.exclude("ALTITUDE_MISSING")
			flush()
			continue
		}
		ground, ok := in.Map.Terrain.ElevationAt(s.X, s.Z)
		if !ok {
			ev.exclude("OUTSIDE_TERRAIN_MODEL")
			flush()
			continue
		}
		offset := *s.Altitude - ground
		if !above {
			offset = -offset
		}
		limit := p.ElevationMinOffset
		if !above {
			limit = p.UndermapMinDepth
		}
		if limit <= 0 || offset < limit {
			flush()
			continue
		}
		if _, ok := in.Map.inZone("STRUCTURE", s.X, s.Z, s.Altitude); ok {
			ev.exclude("STRUCTURE_ZONE")
			flush()
			continue
		}
		if !above {
			if _, ok := in.Map.inZone("UNDERGROUND", s.X, s.Z, s.Altitude); ok {
				ev.exclude("UNDERGROUND_ZONE")
				flush()
				continue
			}
		}
		if above && (!s.VehicleStateKnown || s.InVehicle) {
			// Helicopters and unknown vehicle state are legitimate altitude.
			ev.exclude("VEHICLE_OR_UNKNOWN")
			flush()
			continue
		}
		run, offsets = append(run, s), append(offsets, offset)
	}
	flush()
	return ev.finalize()
}

// NoClipInput requires dense samples and verified collision geometry. Sparse
// samples are excluded, never interpolated into a wall crossing.
type NoClipInput struct {
	Samples []PositionSample
	Map     MapModel
}

func EvaluateNoClip(ctx EvalContext, in NoClipInput) Core8Result {
	ev, blocked := newEvaluation(ctx, "CASE-NOCLIP-001")
	if blocked != nil {
		return *blocked
	}
	if !in.Map.Verified || len(in.Map.Solids) == 0 {
		ev.reasons = append(ev.reasons, "COLLISION_GEOMETRY_UNVERIFIED")
		return ev.finalize()
	}
	p := ctx.Params
	s := orderedSamples(ev, in.Samples)
	for i := 1; i < len(s); i++ {
		a, b := s[i-1], s[i]
		dt := b.EventAt.Sub(a.EventAt)
		switch {
		case dt <= 0:
			ev.exclude("NON_MONOTONIC_TIME")
			continue
		case p.NoClipMaxInterval <= 0 || dt > p.NoClipMaxInterval:
			ev.exclude("SPARSE_SAMPLES")
			continue
		case a.LifeID == "" || a.LifeID != b.LifeID:
			ev.exclude("RESPAWN_OR_LIFE_UNKNOWN")
			continue
		case a.Altitude == nil || b.Altitude == nil:
			ev.exclude("ALTITUDE_MISSING")
			continue
		case !a.VehicleStateKnown || !b.VehicleStateKnown || a.InVehicle || b.InVehicle:
			ev.exclude("VEHICLE_OR_UNKNOWN")
			continue
		}
		if entranceNear(in.Map, a, b) {
			ev.exclude("LEGITIMATE_ENTRANCE")
			continue
		}
		for _, solid := range in.Map.Solids {
			depth, ok := segmentPenetration(solid, a, b)
			if !ok || depth < p.NoClipMinPenetration {
				continue
			}
			ev.observe(Finding{EvidenceIDs: []int64{a.EvidenceID, b.EvidenceID}, EventAt: b.EventAt, ObservedAt: b.ObservedAt,
				Coordinates: []Coordinates{coords(a.X, a.Z, a.Altitude), coords(b.X, b.Z, b.Altitude)},
				Behavior:    "Movement path crossed a solid structure",
				Explanation: fmt.Sprintf("Path between two trusted samples %s apart crossed %.1f m through %q on %s, with no registered entrance on the path.",
					dt.Round(100*time.Millisecond), depth, solid.Label, in.Map.Name)},
				"LEGITIMATE_ENTRANCE", "SPARSE_SAMPLES", "VEHICLE", "RESPAWN")
			break
		}
	}
	return ev.finalize()
}

func entranceNear(m MapModel, a, b PositionSample) bool {
	for _, z := range m.Zones {
		if z.Kind != "ENTRANCE" {
			continue
		}
		if z.contains(a.X, a.Z, a.Altitude) || z.contains(b.X, b.Z, b.Altitude) || pointSegmentDist(z.X, z.Z, a.X, a.Z, b.X, b.Z) <= z.Radius {
			return true
		}
	}
	return false
}

func pointSegmentDist(px, pz, ax, az, bx, bz float64) float64 {
	dx, dz := bx-ax, bz-az
	l2 := dx*dx + dz*dz
	if l2 == 0 {
		return dist2D(px, pz, ax, az)
	}
	t := math.Max(0, math.Min(1, ((px-ax)*dx+(pz-az)*dz)/l2))
	return dist2D(px, pz, ax+t*dx, az+t*dz)
}

// segmentPenetration clips segment a->b against the solid's footprint (slab
// method) and returns the horizontal length inside it when the interpolated
// altitude along that portion lies within the solid's vertical band.
func segmentPenetration(s Solid, a, b PositionSample) (float64, bool) {
	t0, t1 := 0.0, 1.0
	clip := func(p0, d, lo, hi float64) bool {
		if d == 0 {
			return p0 >= lo && p0 <= hi
		}
		ta, tb := (lo-p0)/d, (hi-p0)/d
		if ta > tb {
			ta, tb = tb, ta
		}
		t0, t1 = math.Max(t0, ta), math.Min(t1, tb)
		return t0 <= t1
	}
	if !clip(a.X, b.X-a.X, s.MinX, s.MaxX) || !clip(a.Z, b.Z-a.Z, s.MinZ, s.MaxZ) {
		return 0, false
	}
	altAt := func(t float64) float64 { return *a.Altitude + t*(*b.Altitude-*a.Altitude) }
	lo, hi := math.Min(altAt(t0), altAt(t1)), math.Max(altAt(t0), altAt(t1))
	if hi < s.MinAlt || lo > s.MaxAlt {
		return 0, false
	}
	return (t1 - t0) * dist2D(a.X, a.Z, b.X, b.Z), true
}
