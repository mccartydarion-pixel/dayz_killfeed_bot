package nitradodelivery

import (
	"context"

	"github.com/yourname/dayz-killfeed/internal/shop"
)

// Proposal is the complete non-executing output of a dry run: the plan, the proposed Champion
// spawner file and its diff, the required server action, the rollback procedure and the blockers
// that keep it from running today.
type Proposal struct {
	Plan           Plan
	CurrentSHA256  string
	ProposedFile   []byte
	ProposedSHA256 string
	Diff           string
	RequiredAction string
	Rollback       RollbackPlan
	Steps          []string // every step prefixed "NOT EXECUTED:"
	Blockers       []string
	Executed       bool // always false
}

// PrototypeAdapter implements shop.DeliveryAdapter and is disabled: Enabled() is false and Deliver
// always refuses. Propose builds a Proposal from local inputs only - it has no Nitrado client and
// cannot write a file or restart a server.
type PrototypeAdapter struct{}

var _ shop.DeliveryAdapter = PrototypeAdapter{}

func (PrototypeAdapter) Name() string  { return "nitrado-objectspawner-prototype" }
func (PrototypeAdapter) Enabled() bool { return false }

func (PrototypeAdapter) DryRun(ctx context.Context, plan shop.DeliveryPlan) shop.DryRunReport {
	r := shop.DisabledAdapter{}.DryRun(ctx, plan)
	r.Adapter = "nitrado-objectspawner-prototype"
	return r
}

func (PrototypeAdapter) Deliver(context.Context, shop.DeliveryPlan) error {
	return shop.ErrAutomaticDeliveryDisabled
}

// Blockers are the unverified facts that keep automatic delivery disabled (docs "Remaining blockers").
var Blockers = []string{
	"automatic delivery is disabled in production",
	"the Nitrado upload flow (token + raw POST, from Nitrado's official PHP SDK) has not been exercised against a test server",
	"whether console Nitrado services expose enableCfgGameplayFile and a writable mission folder is unverified",
	"the exact mission-folder path on console Nitrado services is unverified",
	"a verified altitude source for the delivery point is required (the object spawner does not snap to the ground)",
	"item behaviour (quantity, condition, attachments, physics) when created by the object spawner is unverified on console",
	"no automated in-game proof of pickup exists: verification is by the player/staff",
}

// Propose validates the plan against the current Champion file content and returns the dry-run
// proposal. current is the Champion spawner file as it would be downloaded (empty = no file yet).
func (PrototypeAdapter) Propose(p Plan, current []byte) (Proposal, error) {
	cur, err := ParseSpawnerFile(current)
	if err != nil {
		return Proposal{}, err
	}
	next, err := Stage(cur, p)
	if err != nil {
		return Proposal{}, err
	}
	before := cur.Render()
	if len(current) == 0 {
		before = nil
	}
	proposed := next.Render()
	return Proposal{
		Plan: p, CurrentSHA256: SHA256(current), ProposedFile: proposed, ProposedSHA256: SHA256(proposed),
		Diff: Diff(before, proposed), RequiredAction: p.RequiredAction(), Rollback: PlanRollback(p, current, proposed),
		Steps: []string{
			"NOT EXECUTED: record attempt " + p.AttemptID() + " as PLAN_CREATED",
			"NOT EXECUTED: back up and verify " + ArtifactRelPath + " (SHA-256 " + SHA256(current) + ")",
			"NOT EXECUTED: upload the proposed file (SHA-256 " + SHA256(proposed) + ") and verify it -> FILE_STAGED",
			"NOT EXECUTED: ask the server owner for a " + RequiredActionOwnerConfirmedRestart + " -> AWAITING_RESTART",
			"NOT EXECUTED: observe the restart, then unstage attempt " + p.AttemptID() + " at once -> RESTART_OBSERVED",
			"NOT EXECUTED: confirm with the player that the item was collected -> VERIFICATION_REQUIRED -> FULFILLED",
		},
		Blockers: append([]string(nil), Blockers...),
	}, nil
}
