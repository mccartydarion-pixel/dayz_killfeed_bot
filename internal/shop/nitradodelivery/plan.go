// Package nitradodelivery is the Champion Shop Phase 2B research prototype for automatic delivery on
// console DayZ through Nitrado (docs/SHOP_DELIVERY_PHASE2B.md). It is NON-EXECUTING: it builds an
// immutable delivery plan, validates it, generates the proposed DayZ object-spawner file and its diff,
// and describes the server action and rollback a human would have to perform. It has no Nitrado
// client, no HTTP code and no database code - it cannot write a file or restart a server.
//
// Mechanism (verified against Bohemia's published game scripts, scripts/3_game/objectspawner.c and
// cfggameplayhandler.c in BohemiaInteractive/DayZ-Script-Diff): when serverDZ.cfg has
// enableCfgGameplayFile = 1, the server reads $mission:cfggameplay.json at start; every file listed in
// WorldsData.objectSpawnersArr is a JSON {"Objects":[{name,pos[3],ypr[3],scale,enableCEPersistency,
// customString}]} and each entry is created ONCE PER SERVER START at the exact [x, y, z] given. There
// is no reload and no ground snapping, so a staged entry spawns again on every start until it is
// removed, and it needs a real altitude.
package nitradodelivery

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"

	"github.com/yourname/dayz-killfeed/internal/dayzmap"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Artifact location. The spawner file is Champion-owned and lives in its own directory of the
// mission folder; Champion never edits a server-owner file (cfggameplay.json is changed once, by the
// owner, to reference it - see docs). Nothing here accepts a path from a request.
const (
	ArtifactDir      = "champion"
	ArtifactFile     = "champion_shop_delivery.json"
	ArtifactRelPath  = ArtifactDir + "/" + ArtifactFile // relative to the mission folder
	MaxUnitsPerOrder = 10                               // one spawner entry per unit
	MaxStagedObjects = 50                               // per installation file
)

// RequiredAction is what must happen on the server for a staged file to take effect. The spawner only
// runs at server start, so it is always a restart - performed by a human, never by Champion.
const RequiredActionOwnerConfirmedRestart = "OWNER_CONFIRMED_RESTART"

// Altitude bounds: a sanity range only. The real value must come from a verified source (for example
// the player's own ADM-recorded position); Champion never invents one.
const (
	MinAltitude = -50.0
	MaxAltitude = 1500.0
)

var (
	ErrWrongTenant          = errors.New("the delivery does not belong to this organization/installation")
	ErrServiceMismatch      = errors.New("the delivery's game server is not the installation's bound Nitrado service")
	ErrNotCoordinate        = errors.New("only MANUAL_COORDINATE deliveries can be planned")
	ErrNotDeliverable       = errors.New("the delivery is not open (refunded, cancelled or fulfilled): nothing may be staged")
	ErrUnsupportedMap       = errors.New("the map is not supported or differs from the installation's map")
	ErrInvalidCoordinates   = errors.New("coordinates are missing or outside the map")
	ErrAltitudeRequired     = errors.New("a verified altitude (y) is required: the object spawner does not snap to the ground")
	ErrInvalidClassName     = errors.New("invalid item classname")
	ErrClassNameNotApproved = errors.New("the classname is not approved for this product")
	ErrInvalidQuantity      = errors.New("quantity must be 1..10 units")
	ErrInvalidAttempt       = errors.New("attempt number must be >= 1")
	ErrInvalidServiceID     = errors.New("invalid Nitrado service id")
)

// Binding is the installation's verified Nitrado binding, loaded server-side (installations.game_server_id
// -> game_servers.provider_service_id). It carries identifiers only - never a token or password.
type Binding struct {
	OrganizationID   int64
	InstallationID   int64
	GameServerID     int64
	NitradoServiceID string
	MapKey           string // the installation's current delivery map
}

// PlanInput is everything needed to build a plan. ApprovedClassNames is the product's allowlist
// (configured by the owner, never taken from the buyer).
type PlanInput struct {
	OrganizationID, InstallationID int64 // the acting tenant scope
	Binding                        Binding
	Delivery                       repository.ShopDelivery
	ClassName                      string
	ApprovedClassNames             []string
	AltitudeY                      *float64
	Attempt                        int
}

// Plan is an immutable delivery plan: build it with NewPlan, read it through its methods. It never
// holds a credential.
type Plan struct {
	deliveryID, purchaseID, organizationID, installationID, gameServerID int64
	serviceID                                                            string
	mapKey                                                               string
	items                                                                []repository.ShopPurchaseItem
	quantity                                                             int
	x, y, z                                                              float64
	className                                                            string
	attempt                                                              int
}

func (p Plan) DeliveryID() int64        { return p.deliveryID }
func (p Plan) PurchaseID() int64        { return p.purchaseID }
func (p Plan) OrganizationID() int64    { return p.organizationID }
func (p Plan) InstallationID() int64    { return p.installationID }
func (p Plan) GameServerID() int64      { return p.gameServerID }
func (p Plan) NitradoServiceID() string { return p.serviceID }
func (p Plan) MapKey() string           { return p.mapKey }
func (p Plan) Quantity() int            { return p.quantity }
func (p Plan) Position() [3]float64     { return [3]float64{p.x, p.y, p.z} }
func (p Plan) ClassName() string        { return p.className }
func (p Plan) Attempt() int             { return p.attempt }
func (p Plan) RequiredAction() string   { return RequiredActionOwnerConfirmedRestart }
func (p Plan) ArtifactPath() string     { return ArtifactRelPath }

// Items returns a copy of the product snapshot.
func (p Plan) Items() []repository.ShopPurchaseItem {
	return append([]repository.ShopPurchaseItem(nil), p.items...)
}

// AttemptID is the durable identity of this delivery attempt. It is written into every staged
// spawner entry's customString, so a staged object can always be traced back (and removed) by it.
func (p Plan) AttemptID() string {
	return fmt.Sprintf("champion:d%d:a%d", p.deliveryID, p.attempt)
}

// Fingerprint is a stable hash of every planned fact; two plans with the same fingerprint stage the
// same objects.
func (p Plan) Fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%d|%d|%d|%d|%s|%s|%d|%s|%s|%s|%s|%d", p.deliveryID, p.purchaseID, p.organizationID, p.installationID, p.gameServerID,
		p.serviceID, p.mapKey, p.quantity, fmtF(p.x), fmtF(p.y), fmtF(p.z), p.className, p.attempt)
	return hex.EncodeToString(h.Sum(nil))
}

func fmtF(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

var (
	classNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{1,63}$`)
	serviceIDRe = regexp.MustCompile(`^[0-9]{1,20}$`)
)

// ValidateClassName checks a DayZ config classname: letters, digits and underscores only. A name with
// a path separator would make the spawner create a static P3D model instead of an item, so it is
// refused outright.
func ValidateClassName(name string) error {
	if !classNameRe.MatchString(name) {
		return ErrInvalidClassName
	}
	return nil
}

// NewPlan validates every input and returns the immutable plan. The checks are the security
// boundary of any future executor: tenant, installation, Nitrado service identity, open delivery
// (never after a refund), supported and matching map, on-map coordinates, verified altitude,
// approved classname and bounded quantity.
func NewPlan(in PlanInput) (Plan, error) {
	d, b := in.Delivery, in.Binding
	if d.OrganizationID != in.OrganizationID || d.InstallationID != in.InstallationID ||
		b.OrganizationID != in.OrganizationID || b.InstallationID != in.InstallationID {
		return Plan{}, ErrWrongTenant
	}
	if !serviceIDRe.MatchString(b.NitradoServiceID) {
		return Plan{}, ErrInvalidServiceID
	}
	if d.GameServerID == nil || *d.GameServerID != b.GameServerID || b.GameServerID <= 0 {
		return Plan{}, ErrServiceMismatch
	}
	if d.Policy != repository.DeliveryPolicyManualCoordinate {
		return Plan{}, ErrNotCoordinate
	}
	if d.Status != repository.DeliveryStatusManualReady || d.PurchaseStatus != repository.ShopStatusPendingFulfillment {
		return Plan{}, ErrNotDeliverable
	}
	m, ok := dayzmap.Lookup(d.MapKey)
	if !ok || d.MapKey != b.MapKey {
		return Plan{}, ErrUnsupportedMap
	}
	if d.X == nil || d.Z == nil || !m.Contains(*d.X, *d.Z) {
		return Plan{}, ErrInvalidCoordinates
	}
	if in.AltitudeY == nil || math.IsNaN(*in.AltitudeY) || math.IsInf(*in.AltitudeY, 0) || *in.AltitudeY < MinAltitude || *in.AltitudeY > MaxAltitude {
		return Plan{}, ErrAltitudeRequired
	}
	if err := ValidateClassName(in.ClassName); err != nil {
		return Plan{}, err
	}
	approved := false
	for _, c := range in.ApprovedClassNames {
		if c == in.ClassName {
			approved = true
			break
		}
	}
	if !approved {
		return Plan{}, ErrClassNameNotApproved
	}
	qty := 0
	for _, it := range d.Items {
		qty += it.Quantity
	}
	if len(d.Items) != 1 || qty < 1 || qty > MaxUnitsPerOrder {
		return Plan{}, ErrInvalidQuantity
	}
	if in.Attempt < 1 {
		return Plan{}, ErrInvalidAttempt
	}
	return Plan{deliveryID: d.ID, purchaseID: d.PurchaseID, organizationID: d.OrganizationID, installationID: d.InstallationID,
		gameServerID: b.GameServerID, serviceID: b.NitradoServiceID, mapKey: m.Key, items: append([]repository.ShopPurchaseItem(nil), d.Items...),
		quantity: qty, x: *d.X, y: *in.AltitudeY, z: *d.Z, className: in.ClassName, attempt: in.Attempt}, nil
}
