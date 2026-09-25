package canary

import (
	"errors"

	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

// The canary is exactly one BandageDressing: a small, common, harmless item whose appearance on the
// ground is easy to confirm and costs nothing if it appears twice.
const (
	CanaryClassName = "BandageDressing"
	CanaryQuantity  = 1
	// PreviewDeliveryID marks a plan built from a synthetic, never-stored delivery. The real canary
	// needs its own delivery record, created only under an approved gate.
	PreviewDeliveryID int64 = 0
)

var ErrDropPointNotVerified = errors.New("the drop point is not verified for the current session")

// CanaryInput is the binding, the verified drop point and the delivery identity. DeliveryID 0 builds a
// labelled preview.
type CanaryInput struct {
	Binding    nitradodelivery.Binding
	DropPoint  DropPoint
	Check      DropPointCheck
	DeliveryID int64
	Attempt    int
}

// CanaryPreview is everything the single-item test would write, computed without writing.
type CanaryPreview struct {
	Preview        bool // true: built from a synthetic delivery; the attempt id changes with the real record
	AttemptID      string
	Fingerprint    string
	ClassName      string
	Quantity       int
	Pos            [3]float64
	Entries        []nitradodelivery.SpawnerObject
	StagedJSON     []byte // the Champion spawner file content Gate B uploads
	StagedSHA256   string
	UnstagedJSON   []byte // the content Gate D uploads (same as the rollback content)
	UnstagedSHA256 string
	StageDiff      string // absent/empty file -> staged
	UnstageDiff    string // staged -> unstaged
	Rollback       nitradodelivery.RollbackPlan
}

// PreviewSingleItem builds the BandageDressing x1 plan through the production plan validator
// (nitradodelivery.NewPlan) at a verified drop point.
func PreviewSingleItem(in CanaryInput) (CanaryPreview, error) {
	if in.Check.Status != DropPointVerified {
		return CanaryPreview{}, ErrDropPointNotVerified
	}
	dp, b := in.DropPoint, in.Binding
	gs := b.GameServerID
	x, z, y := dp.X, dp.Z, *dp.AltitudeY
	attempt := in.Attempt
	if attempt == 0 {
		attempt = 1
	}
	d := repository.ShopDelivery{
		ID: in.DeliveryID, OrganizationID: b.OrganizationID, InstallationID: b.InstallationID, GameServerID: &gs,
		PlayerName: "canary", DeliveryType: "MANUAL", Policy: repository.DeliveryPolicyManualCoordinate, MapKey: dp.MapKey, X: &x, Z: &z,
		Status: repository.DeliveryStatusManualReady, PurchaseStatus: repository.ShopStatusPendingFulfillment,
		Items: []repository.ShopPurchaseItem{{ProductName: "Canary: " + CanaryClassName, Quantity: CanaryQuantity}},
	}
	plan, err := nitradodelivery.NewPlan(nitradodelivery.PlanInput{
		OrganizationID: dp.OrganizationID, InstallationID: dp.InstallationID, Binding: b, Delivery: d,
		ClassName: CanaryClassName, ApprovedClassNames: []string{CanaryClassName}, AltitudeY: &y, Attempt: attempt,
	})
	if err != nil {
		return CanaryPreview{}, err
	}
	empty := nitradodelivery.SpawnerFile{Objects: []nitradodelivery.SpawnerObject{}}
	staged, err := nitradodelivery.Stage(empty, plan)
	if err != nil {
		return CanaryPreview{}, err
	}
	stagedB := staged.Render()
	unstagedB := nitradodelivery.Unstage(staged, plan.AttemptID()).Render()
	return CanaryPreview{
		Preview: in.DeliveryID == PreviewDeliveryID, AttemptID: plan.AttemptID(), Fingerprint: plan.Fingerprint(),
		ClassName: plan.ClassName(), Quantity: plan.Quantity(), Pos: plan.Position(), Entries: plan.Entries(),
		StagedJSON: stagedB, StagedSHA256: nitradodelivery.SHA256(stagedB),
		UnstagedJSON: unstagedB, UnstagedSHA256: nitradodelivery.SHA256(unstagedB),
		StageDiff: nitradodelivery.Diff(nil, stagedB), UnstageDiff: nitradodelivery.Diff(stagedB, unstagedB),
		Rollback: nitradodelivery.PlanRollback(plan, unstagedB, stagedB),
	}, nil
}
