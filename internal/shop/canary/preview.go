package canary

import (
	"errors"
	"math"
	"strings"

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

// DeliveryMatchTolerance: the canary purchase's X/Z (typed by the owner in the normal purchase flow)
// must be this close to the verified drop point, whose altitude the plan uses.
const DeliveryMatchTolerance = 0.5

var (
	ErrDropPointNotVerified  = errors.New("the drop point is not verified for the current session")
	ErrPlaceholderAttempt    = errors.New("a preview (placeholder delivery 0) attempt id can never be used in production")
	ErrCanaryDeliveryShape   = errors.New("the canary delivery must be exactly one item, quantity 1")
	ErrCanaryDeliveryFarAway = errors.New("the canary delivery's coordinates are not the verified drop point")
)

// CanaryInput is the binding, the verified drop point and, for a real plan, the canary delivery created
// through the existing Shop purchase flow. Without Delivery a labelled PREVIEW is built on a synthetic,
// never-stored delivery with id 0.
type CanaryInput struct {
	Binding   nitradodelivery.Binding
	DropPoint DropPoint
	Check     DropPointCheck
	Delivery  *repository.ShopDelivery
	Attempt   int
}

// ValidateProductionAttemptID refuses the preview identity (delivery 0) and anything that is not a
// Champion attempt id.
func ValidateProductionAttemptID(id string) error {
	if strings.HasPrefix(id, "champion:d0:") || !strings.HasPrefix(id, "champion:d") {
		return ErrPlaceholderAttempt
	}
	return nil
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
	StagedJSON     []byte // the Champion spawner file content Gate E uploads
	StagedSHA256   string
	UnstagedJSON   []byte // the content Gate G restores (= the Gate A empty file = the rollback content)
	UnstagedSHA256 string
	StageDiff      string // empty Champion file (Gate A) -> staged
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
	y := *dp.AltitudeY
	attempt := in.Attempt
	if attempt == 0 {
		attempt = 1
	}
	var d repository.ShopDelivery
	if in.Delivery != nil {
		d = *in.Delivery
		if d.ID <= 0 {
			return CanaryPreview{}, ErrPlaceholderAttempt
		}
		if len(d.Items) != 1 || d.Items[0].Quantity != CanaryQuantity {
			return CanaryPreview{}, ErrCanaryDeliveryShape
		}
		if d.X == nil || d.Z == nil || math.Abs(*d.X-dp.X) > DeliveryMatchTolerance || math.Abs(*d.Z-dp.Z) > DeliveryMatchTolerance {
			return CanaryPreview{}, ErrCanaryDeliveryFarAway
		}
	} else {
		gs := b.GameServerID
		x, z := dp.X, dp.Z
		d = repository.ShopDelivery{
			ID: PreviewDeliveryID, OrganizationID: b.OrganizationID, InstallationID: b.InstallationID, GameServerID: &gs,
			PlayerName: "canary", DeliveryType: "MANUAL", Policy: repository.DeliveryPolicyManualCoordinate, MapKey: dp.MapKey, X: &x, Z: &z,
			Status: repository.DeliveryStatusManualReady, PurchaseStatus: repository.ShopStatusPendingFulfillment,
			Items: []repository.ShopPurchaseItem{{ProductName: "Canary: " + CanaryClassName, Quantity: CanaryQuantity}},
		}
	}
	plan, err := nitradodelivery.NewPlan(nitradodelivery.PlanInput{
		OrganizationID: dp.OrganizationID, InstallationID: dp.InstallationID, Binding: b, Delivery: d,
		ClassName: CanaryClassName, ApprovedClassNames: []string{CanaryClassName}, AltitudeY: &y, Attempt: attempt,
	})
	if err != nil {
		return CanaryPreview{}, err
	}
	empty := nitradodelivery.SpawnerFile{Objects: []nitradodelivery.SpawnerObject{}}
	emptyB := empty.Render()
	staged, err := nitradodelivery.Stage(empty, plan)
	if err != nil {
		return CanaryPreview{}, err
	}
	stagedB := staged.Render()
	unstagedB := nitradodelivery.Unstage(staged, plan.AttemptID()).Render()
	return CanaryPreview{
		Preview: in.Delivery == nil, AttemptID: plan.AttemptID(), Fingerprint: plan.Fingerprint(),
		ClassName: plan.ClassName(), Quantity: plan.Quantity(), Pos: plan.Position(), Entries: plan.Entries(),
		StagedJSON: stagedB, StagedSHA256: nitradodelivery.SHA256(stagedB),
		UnstagedJSON: unstagedB, UnstagedSHA256: nitradodelivery.SHA256(unstagedB),
		StageDiff: nitradodelivery.Diff(emptyB, stagedB), UnstageDiff: nitradodelivery.Diff(stagedB, unstagedB),
		Rollback: nitradodelivery.PlanRollback(plan, unstagedB, stagedB),
	}, nil
}
