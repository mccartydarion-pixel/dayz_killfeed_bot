package caseintel

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// BuildAction is a retained, source-addressed build line. FactionID is the
// actor's faction AT EVENT TIME; nil means membership is unknown.
type BuildAction struct {
	TimedEvidence
	Action    string
	Object    string
	X, Z      float64
	Altitude  *float64
	FactionID *int64
}

// RegisteredBase is an owner registration (see draft migration 0070). A base
// only protects actions after RegisteredAt, and only when Verified.
type RegisteredBase struct {
	BaseID             string
	Verified           bool
	OwnerPlayerID      int64
	OwnerFactionID     *int64
	X, Z, Radius       float64
	RegisteredAt       time.Time
	AuthorizedPlayers  []int64
	AuthorizedFactions []int64
}

type BaseBoostInput struct {
	Actions []BuildAction
	Bases   []RegisteredBase
}

// EvaluateBaseBoost looks for construction by an unauthorized player inside
// another player's registered base radius. Boosting here means unauthorized
// base construction, never kill farming or statistical boosting.
func EvaluateBaseBoost(ctx EvalContext, in BaseBoostInput) Core8Result {
	ev, blocked := newEvaluation(ctx, "CASE-BASE-001")
	if blocked != nil {
		return *blocked
	}
	p := ctx.Params
	for _, act := range in.Actions {
		if prob := act.sampleProblem(ctx); prob != "" {
			ev.exclude(prob)
			continue
		}
		if strings.TrimSpace(act.Action) == "" || strings.TrimSpace(act.Object) == "" {
			ev.exclude("BUILD_ACTION_INCOMPLETE")
			continue
		}
		for _, base := range in.Bases {
			radius := base.Radius
			if radius < p.BaseMinRadius {
				radius = p.BaseMinRadius
			}
			if p.BaseMaxRadius > 0 && radius > p.BaseMaxRadius {
				radius = p.BaseMaxRadius
			}
			if dist2D(base.X, base.Z, act.X, act.Z) > radius {
				continue
			}
			switch {
			case !base.Verified || base.OwnerPlayerID <= 0:
				ev.exclude("BASE_UNVERIFIED")
				continue
			case base.RegisteredAt.IsZero() || act.EventAt.Before(base.RegisteredAt):
				ev.exclude("NOT_REGISTERED_AT_EVENT_TIME")
				continue
			case act.PlayerID == base.OwnerPlayerID || containsID(base.AuthorizedPlayers, act.PlayerID):
				ev.exclude("AUTHORIZED_PLAYER")
				continue
			}
			if base.OwnerFactionID != nil || len(base.AuthorizedFactions) > 0 {
				if act.FactionID == nil {
					ev.exclude("FACTION_MEMBERSHIP_UNKNOWN")
					continue
				}
				if (base.OwnerFactionID != nil && *act.FactionID == *base.OwnerFactionID) || containsID(base.AuthorizedFactions, *act.FactionID) {
					ev.exclude("AUTHORIZED_FACTION")
					continue
				}
			}
			behavior := "Built inside someone else's base"
			if isElevatedStructure(act.Object) {
				behavior = "Put up a watchtower inside someone else's base"
			}
			ev.observe(Finding{EvidenceIDs: []int64{act.EvidenceID}, EventAt: act.EventAt, ObservedAt: act.ObservedAt,
				Coordinates: []Coordinates{coords(act.X, act.Z, act.Altitude)}, AffectedBaseID: base.BaseID, Behavior: behavior,
				Explanation: fmt.Sprintf("%s %s about %.0f m from the centre of base %s. They aren't the owner, on the base's friend list, or in an allowed faction.",
					act.Action, act.Object, dist2D(base.X, base.Z, act.X, act.Z), base.BaseID)},
				"BASE_OWNER", "AUTHORIZED_PLAYER", "AUTHORIZED_FACTION", "REGISTERED_AT_EVENT_TIME")
			break
		}
	}
	return ev.finalize()
}

func isElevatedStructure(object string) bool {
	o := strings.ToLower(object)
	return strings.Contains(o, "watchtower") || strings.Contains(o, "tower") || strings.Contains(o, "platform")
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// SessionEvent is a CONNECT or DISCONNECT with a trusted event time.
type SessionEvent struct {
	TimedEvidence
	Kind string
}

// ItemEvent is item-level evidence with a persistent item identity and
// verified transaction provenance. Kind is ACQUIRED or RELEASED.
type ItemEvent struct {
	TimedEvidence
	ItemID             string
	ItemType           string
	Kind               string
	ProvenanceVerified bool
}

type DupeInput struct {
	Sessions []SessionEvent
	Restarts []RestartWindow
	Items    []ItemEvent
}

// EvaluateDupe requires item-level evidence: one persistent item identity
// acquired twice without an intervening release, around a reconnect or
// restart. Reconnect or restart timing alone never becomes an observation.
func EvaluateDupe(ctx EvalContext, in DupeInput) Core8Result {
	ev, blocked := newEvaluation(ctx, "CASE-DUPE-001")
	if blocked != nil {
		return *blocked
	}
	p := ctx.Params
	var items []ItemEvent
	for _, it := range in.Items {
		if prob := it.sampleProblem(ctx); prob != "" {
			ev.exclude(prob)
			continue
		}
		if !it.ProvenanceVerified || strings.TrimSpace(it.ItemID) == "" {
			ev.exclude("ITEM_PROVENANCE_UNVERIFIED")
			continue
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		ev.reasons = append(ev.reasons, "ITEM_EVIDENCE_UNAVAILABLE")
		return ev.finalize()
	}
	var reconnects []time.Time
	for _, s := range in.Sessions {
		if s.sampleProblem(ctx) == "" && s.Kind == "CONNECT" {
			reconnects = append(reconnects, s.EventAt)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].EventAt.Before(items[j].EventAt) })
	held := map[string]ItemEvent{}
	for _, it := range items {
		switch it.Kind {
		case "RELEASED":
			delete(held, it.ItemID)
		case "ACQUIRED":
			first, dup := held[it.ItemID]
			if !dup {
				held[it.ItemID] = it
				continue
			}
			context := ""
			for _, r := range reconnects {
				if !r.Before(first.EventAt.Add(-p.DupeCorrelationWindow)) && !r.After(it.EventAt.Add(p.DupeCorrelationWindow)) {
					context = "a reconnect"
				}
			}
			if context == "" && spansRestart(in.Restarts, p.DupeCorrelationWindow, first.EventAt, it.EventAt) {
				context = "a server restart"
			}
			if context == "" {
				ev.exclude("NO_RECONNECT_OR_RESTART_CONTEXT")
				continue
			}
			ev.observe(Finding{EvidenceIDs: []int64{first.EvidenceID, it.EvidenceID}, EventAt: it.EventAt, ObservedAt: it.ObservedAt,
				Behavior: "Same item picked up twice around " + context,
				Explanation: fmt.Sprintf("Item %s (%s) was picked up at %s and again at %s without being dropped in between, around %s. Staff should check whether it was really duplicated.",
					it.ItemID, it.ItemType, first.EventAt.UTC().Format(time.RFC3339), it.EventAt.UTC().Format(time.RFC3339), context)},
				"ITEM_RELEASE", "ITEM_PROVENANCE", "RECONNECT_OR_RESTART_CONTEXT")
		default:
			ev.exclude("UNKNOWN_ITEM_EVENT")
		}
	}
	return ev.finalize()
}

// PlatformAttestation is trusted, signature-verified client platform data.
// There is deliberately no field for names, movement or input behaviour:
// those can never identify a PC client.
type PlatformAttestation struct {
	TimedEvidence
	Platform          string // XBOX, PC, ...
	Source            string
	Trusted           bool
	SignatureVerified bool
}

type PCDetectionInput struct {
	InstallationPlatform string
	Attestations         []PlatformAttestation
}

// EvaluatePCXbox reports UNSUPPORTED unless trusted attestation exists for an
// Xbox installation. An attested PC platform is the only observation.
func EvaluatePCXbox(ctx EvalContext, in PCDetectionInput) Core8Result {
	ev, blocked := newEvaluation(ctx, "CASE-PC-XBOX-001")
	if blocked != nil {
		return *blocked
	}
	if strings.ToUpper(in.InstallationPlatform) != "XBOX" {
		res := ev.finalize()
		res.Status = "UNSUPPORTED"
		res.Reasons = uniqueSorted(append(res.Reasons, "NOT_XBOX_INSTALLATION"))
		return res
	}
	trusted := 0
	for _, a := range in.Attestations {
		if prob := a.sampleProblem(ctx); prob != "" {
			ev.exclude(prob)
			continue
		}
		if !a.Trusted || !a.SignatureVerified || strings.TrimSpace(a.Source) == "" {
			ev.exclude("ATTESTATION_UNTRUSTED")
			continue
		}
		trusted++
		switch strings.ToUpper(a.Platform) {
		case "XBOX":
		case "PC", "WINDOWS", "STEAM":
			ev.observe(Finding{EvidenceIDs: []int64{a.EvidenceID}, EventAt: a.EventAt, ObservedAt: a.ObservedAt,
				Behavior:    "A PC player on an Xbox server",
				Explanation: fmt.Sprintf("The platform check from %s says this player is on %s.", a.Source, a.Platform)},
				"ATTESTATION_SIGNATURE", "ATTESTATION_SOURCE")
		default:
			ev.exclude("PLATFORM_UNRECOGNIZED")
		}
	}
	res := ev.finalize()
	if trusted == 0 {
		res.Status = "UNSUPPORTED"
		res.Reasons = uniqueSorted(append(res.Reasons, "TRUSTED_PLATFORM_ATTESTATION_UNAVAILABLE"))
	}
	return res
}

type LoginInput struct {
	Sessions            []SessionEvent
	Restarts            []RestartWindow
	RelatedIncidentKeys []string
}

// EvaluateSuspiciousLogins groups rapid disconnect->connect pairs and repeated
// connects without a disconnect into bursts. Reconnects around a verified
// restart are excluded. A burst is a security review item, not a cheat.
func EvaluateSuspiciousLogins(ctx EvalContext, in LoginInput) Core8Result {
	ev, blocked := newEvaluation(ctx, "CASE-LOGIN-001")
	if blocked != nil {
		return *blocked
	}
	p := ctx.Params
	var sessions []SessionEvent
	seen := map[int64]bool{}
	for _, s := range in.Sessions {
		if prob := s.sampleProblem(ctx); prob != "" {
			ev.exclude(prob)
			continue
		}
		if seen[s.EvidenceID] {
			ev.exclude("DUPLICATE_SAMPLE")
			continue
		}
		seen[s.EvidenceID] = true
		sessions = append(sessions, s)
	}
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].EventAt.Before(sessions[j].EventAt) })
	type abnormal struct {
		ids []int64
		at  time.Time
	}
	var events []abnormal
	for i := 1; i < len(sessions); i++ {
		a, b := sessions[i-1], sessions[i]
		if b.Kind != "CONNECT" {
			continue
		}
		if withinRestart(in.Restarts, p.RestartGrace, a.EventAt, b.EventAt) {
			ev.exclude("SERVER_RESTART")
			continue
		}
		// A connect->connect pair far apart is more likely a missing
		// disconnect line than abnormal access, so both use the same gap.
		if b.EventAt.Sub(a.EventAt) <= p.LoginReconnectGap {
			events = append(events, abnormal{[]int64{a.EvidenceID, b.EvidenceID}, b.EventAt})
		}
	}
	used := map[int64]bool{}
	for start := 0; start < len(events); {
		end := start
		for end+1 < len(events) && events[end+1].at.Sub(events[start].at) <= p.LoginBurstWindow {
			end++
		}
		n := end - start + 1
		if p.LoginMinReconnects <= 0 || n < p.LoginMinReconnects {
			ev.exclude("ORDINARY_RECONNECTS")
			start = end + 1
			continue
		}
		f := Finding{EventAt: events[end].at, RelatedIncidentKeys: append([]string(nil), in.RelatedIncidentKeys...),
			Behavior: "Reconnected many times in a short time"}
		// A boundary connect shared with an earlier burst stays with that burst.
		for _, e := range events[start : end+1] {
			for _, id := range e.ids {
				if !used[id] {
					used[id] = true
					f.EvidenceIDs = append(f.EvidenceIDs, id)
				}
			}
		}
		if len(f.EvidenceIDs) == 0 {
			start = end + 1
			continue
		}
		prefix := p.LoginMinReconnects
		if prefix > len(f.EvidenceIDs) {
			prefix = len(f.EvidenceIDs)
		}
		f.keyEvidenceIDs = append([]int64(nil), f.EvidenceIDs[:prefix]...)
		for _, s := range sessions {
			if s.EvidenceID == f.EvidenceIDs[len(f.EvidenceIDs)-1] {
				f.ObservedAt = s.ObservedAt
			}
		}
		f.Explanation = fmt.Sprintf("Reconnected %d times in %s, not during a server restart. Often this is just a bad connection, so it's worth a quick look before assuming anything.", n, plainDuration(p.LoginBurstWindow))
		ev.observe(f, "SERVER_RESTART", "ORDINARY_RECONNECT")
		start = end + 1
	}
	return ev.finalize()
}
