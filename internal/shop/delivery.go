package shop

import (
	"context"
	"errors"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/dayzmap"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Delivery Engine 2.0 (docs/SHOP_DELIVERY.md): every purchase has one persistent delivery record.
// Staff fulfill it (POST .../purchases/{id}/fulfill) or it is cancelled by a refund; there is no
// automatic delivery in Phase 2A.

// Delivery statuses (re-exported).
const (
	DeliveryReady     = repository.DeliveryStatusManualReady
	DeliveryFulfilled = repository.DeliveryStatusFulfilled
	DeliveryCancelled = repository.DeliveryStatusCancelled
)

var deliveryStatuses = map[string]bool{DeliveryReady: true, DeliveryFulfilled: true, DeliveryCancelled: true}

var (
	ErrInvalidDeliveryStatus = errors.New("unknown delivery status")
	ErrInvalidDeliveryPolicy = errors.New("unknown delivery policy")
	// ErrUnsupportedMap is a map key that is not in the verified catalog (internal/dayzmap).
	ErrUnsupportedMap = errors.New("unsupported map")
)

// DeliveryList filters the delivery queue.
type DeliveryList struct {
	Status   string
	Policy   string
	PlayerID int64
	Limit    int
	Cursor   string
}

// DeliveryPage is one page of the queue, newest first; NextCursor is "" on the last page.
type DeliveryPage struct {
	Items      []repository.ShopDelivery
	NextCursor string
	Limit      int
}

const deliveryCursorPrefix = "sdc1:"

// AdminDeliveries is the installation's delivery queue (organization OWNER/ADMIN only).
func (s *Service) AdminDeliveries(ctx context.Context, scope repository.EconomyScope, q DeliveryList) (DeliveryPage, error) {
	if q.Limit <= 0 {
		q.Limit = DefaultLimit
	}
	if q.Limit > MaxLimit {
		q.Limit = MaxLimit
	}
	status := strings.ToUpper(strings.TrimSpace(q.Status))
	if status != "" && !deliveryStatuses[status] {
		return DeliveryPage{}, ErrInvalidDeliveryStatus
	}
	policy := strings.ToUpper(strings.TrimSpace(q.Policy))
	if policy != "" && policy != PolicyManualPickup && policy != PolicyManualCoordinate {
		return DeliveryPage{}, ErrInvalidDeliveryPolicy
	}
	var before int64
	if q.Cursor != "" {
		id, ok := decodeCursor(deliveryCursorPrefix, q.Cursor)
		if !ok {
			return DeliveryPage{}, ErrInvalidCursor
		}
		before = id
	}
	items, next, err := s.store.ListDeliveries(ctx, scope.OrganizationID, scope.InstallationID, repository.DeliveryQuery{Status: status, Policy: policy, PlayerID: q.PlayerID, Limit: q.Limit, BeforeID: before})
	if err != nil {
		return DeliveryPage{}, err
	}
	page := DeliveryPage{Items: items, Limit: q.Limit}
	if page.Items == nil {
		page.Items = []repository.ShopDelivery{}
	}
	if next != 0 {
		page.NextCursor = encodeCursor(deliveryCursorPrefix, next)
	}
	return page, nil
}

// AdminDelivery reads one delivery of the installation.
func (s *Service) AdminDelivery(ctx context.Context, scope repository.EconomyScope, id int64) (*repository.ShopDelivery, error) {
	return s.store.GetDelivery(ctx, scope.OrganizationID, scope.InstallationID, id, 0)
}

// MyDelivery reads one of the acting user's own deliveries (their VERIFIED player only); anyone
// else's is "not found".
func (s *Service) MyDelivery(ctx context.Context, scope repository.EconomyScope, discordUserID string, id int64) (*repository.ShopDelivery, error) {
	acct, err := s.identity.Me(ctx, scope, discordUserID)
	if err != nil {
		return nil, err
	}
	return s.store.GetDelivery(ctx, scope.OrganizationID, scope.InstallationID, id, acct.AccountID)
}

// DeliverySettings is the installation's delivery configuration with its resolved map.
type DeliverySettings struct {
	Map      *dayzmap.Map // nil = not configured or no longer supported
	MapKey   string       // the stored key, even if unsupported
	Settings repository.ShopDeliverySettings
}

func settingsOf(s repository.ShopDeliverySettings) DeliverySettings {
	out := DeliverySettings{MapKey: s.MapKey, Settings: s}
	if m, ok := dayzmap.Lookup(s.MapKey); ok {
		out.Map = &m
	}
	return out
}

// DeliverySettings returns the installation's delivery map (readable by any player: the website
// needs the bounds to render coordinate input).
func (s *Service) DeliverySettings(ctx context.Context, scope repository.EconomyScope) (DeliverySettings, error) {
	st, err := s.store.GetDeliverySettings(ctx, scope.OrganizationID, scope.InstallationID)
	if err != nil {
		return DeliverySettings{}, err
	}
	return settingsOf(st), nil
}

// SetDeliveryMap sets (or, with "", clears) the installation's map. Only verified maps are
// accepted. Existing deliveries keep the map they were bought with.
func (s *Service) SetDeliveryMap(ctx context.Context, scope repository.EconomyScope, actorUserID int64, mapKey string) (DeliverySettings, error) {
	key := strings.ToLower(strings.TrimSpace(mapKey))
	if key != "" {
		m, ok := dayzmap.Lookup(key)
		if !ok {
			return DeliverySettings{}, ErrUnsupportedMap
		}
		key = m.Key
	}
	st, err := s.store.SetDeliveryMap(ctx, scope.OrganizationID, scope.InstallationID, key, actorUserID)
	if err != nil {
		return DeliverySettings{}, err
	}
	return settingsOf(st), nil
}

// --- automatic delivery (Phase 2B): interface only, disabled -------------------------------------

// ErrAutomaticDeliveryDisabled is returned by every automatic-delivery path in Phase 2A.
var ErrAutomaticDeliveryDisabled = errors.New("automatic delivery is disabled: deliveries are fulfilled by staff")

// DeliveryPlan is what an automatic adapter would be asked to deliver. It carries no credential.
type DeliveryPlan struct {
	DeliveryID, PurchaseID, GameServerID int64
	MapKey                               string
	X, Z                                 float64
	Items                                []repository.ShopPurchaseItem
}

// DryRunReport describes what an adapter would do, without doing any of it.
type DryRunReport struct {
	Adapter  string
	Enabled  bool
	Steps    []string // human-readable planned steps, all NOT executed
	Blockers []string // why the plan could not run today
}

// DeliveryAdapter is the future automatic delivery boundary (for example a Nitrado mission-file
// adapter). Phase 2A ships only DisabledAdapter: nothing implements a live delivery, and no code
// path calls Deliver.
type DeliveryAdapter interface {
	Name() string
	Enabled() bool
	// DryRun explains a plan with no side effects: no network call, no file write, no restart.
	DryRun(ctx context.Context, plan DeliveryPlan) DryRunReport
	// Deliver would perform the delivery. DisabledAdapter always refuses.
	Deliver(ctx context.Context, plan DeliveryPlan) error
}

// DisabledAdapter is the only adapter in Phase 2A.
type DisabledAdapter struct{}

func (DisabledAdapter) Name() string  { return "disabled" }
func (DisabledAdapter) Enabled() bool { return false }

func (DisabledAdapter) DryRun(_ context.Context, plan DeliveryPlan) DryRunReport {
	r := DryRunReport{Adapter: "disabled", Enabled: false,
		Blockers: []string{
			"automatic delivery is disabled in Phase 2A",
			"no verified Nitrado file-upload endpoint is implemented (read-only file listing/download exists)",
			"no verified in-game spawn mechanism for console DayZ",
		}}
	if plan.MapKey != "" {
		r.Steps = append(r.Steps, "NOT EXECUTED: resolve map "+plan.MapKey)
	}
	r.Steps = append(r.Steps, "NOT EXECUTED: hand the order to staff (MANUAL_READY)")
	return r
}

func (DisabledAdapter) Deliver(context.Context, DeliveryPlan) error {
	return ErrAutomaticDeliveryDisabled
}
