package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/dayzmap"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop"
)

// Champion Shop Delivery Engine 2.0 API (docs/SHOP_DELIVERY.md). Delivery is performed by staff:
// the queue lists what to hand over (and where, for MANUAL_COORDINATE products); fulfillment and
// cancellation go through the existing purchase fulfill/refund routes. Nothing here contacts a
// game server.

const (
	codeDeliveryCoordinatesRequired    = "DELIVERY_COORDINATES_REQUIRED"
	codeDeliveryCoordinatesInvalid     = "DELIVERY_COORDINATES_INVALID"
	codeDeliveryCoordinatesOutOfBounds = "DELIVERY_COORDINATES_OUT_OF_BOUNDS"
	codeDeliveryCoordinatesNotAccepted = "DELIVERY_COORDINATES_NOT_ACCEPTED"
	codeDeliveryMapUnresolved          = "DELIVERY_MAP_UNRESOLVED"
	codeDeliveryNotFound               = "DELIVERY_NOT_FOUND"
	codeUnsupportedMap                 = "UNSUPPORTED_MAP"
)

func init() {
	httpStatusForCode[codeDeliveryCoordinatesRequired] = http.StatusBadRequest
	httpStatusForCode[codeDeliveryCoordinatesInvalid] = http.StatusBadRequest
	httpStatusForCode[codeDeliveryCoordinatesOutOfBounds] = http.StatusBadRequest
	httpStatusForCode[codeDeliveryCoordinatesNotAccepted] = http.StatusBadRequest
	httpStatusForCode[codeDeliveryMapUnresolved] = http.StatusConflict
	httpStatusForCode[codeDeliveryNotFound] = http.StatusNotFound
	httpStatusForCode[codeUnsupportedMap] = http.StatusBadRequest
}

func (a *App) registerShopDeliveryRoutes(base string) {
	h := a.HTTPServer.Handle
	// Player routes.
	h("GET "+base+"/delivery-settings", a.handleShopDeliverySettings)
	h("GET "+base+"/me/deliveries/{deliveryID}", a.handleShopMyDelivery)
	// Admin routes (organization OWNER/ADMIN).
	h("GET "+base+"/admin/delivery-settings", a.handleShopAdminDeliverySettings)
	h("PUT "+base+"/delivery-settings", a.handleShopSetDeliverySettings)
	h("GET "+base+"/admin/deliveries", a.handleShopAdminDeliveries)
	h("GET "+base+"/admin/deliveries/{deliveryID}", a.handleShopAdminDelivery)
}

// shopDeliveryFailed maps delivery errors; false = not a delivery error.
func shopDeliveryFailed(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, repository.ErrShopDeliveryCoordinatesRequired):
		writeSaaSError(w, codeDeliveryCoordinatesRequired, "this product is delivered to map coordinates: send delivery.x and delivery.z")
	case errors.Is(err, shop.ErrInvalidCoordinates):
		writeSaaSError(w, codeDeliveryCoordinatesInvalid, "delivery needs both x and z as finite numbers")
	case errors.Is(err, repository.ErrShopDeliveryOutOfBounds):
		writeSaaSError(w, codeDeliveryCoordinatesOutOfBounds, "the delivery coordinates are outside this server's map - check GET .../shop/delivery-settings for the bounds")
	case errors.Is(err, repository.ErrShopDeliveryCoordinatesNotAccepted):
		writeSaaSError(w, codeDeliveryCoordinatesNotAccepted, "this product is a manual pickup: remove the delivery coordinates")
	case errors.Is(err, repository.ErrShopDeliveryMapUnresolved):
		writeSaaSError(w, codeDeliveryMapUnresolved, "coordinate delivery is unavailable: an admin must set this server's map to a supported map (Chernarus or Livonia) in the shop delivery settings")
	case errors.Is(err, repository.ErrShopDeliveryNotFound):
		writeSaaSError(w, codeDeliveryNotFound, "delivery not found")
	case errors.Is(err, shop.ErrUnsupportedMap):
		writeSaaSError(w, codeUnsupportedMap, "unsupported map: only maps with verified bounds (chernarusplus, enoch) can be used for coordinate delivery")
	case errors.Is(err, shop.ErrInvalidDeliveryStatus):
		writeSaaSError(w, codeInvalidRequest, "status must be MANUAL_READY, FULFILLED or CANCELLED")
	case errors.Is(err, shop.ErrInvalidDeliveryPolicy):
		writeSaaSError(w, codeInvalidRequest, "policy must be MANUAL_PICKUP or MANUAL_COORDINATE")
	default:
		return false
	}
	return true
}

// --- DTOs -------------------------------------------------------------------------------------------

type shopMapDTO struct {
	Key  string  `json:"key"`
	Name string  `json:"name"`
	MinX float64 `json:"minX"`
	MaxX float64 `json:"maxX"`
	MinZ float64 `json:"minZ"`
	MaxZ float64 `json:"maxZ"`
}

func mapDTO(m dayzmap.Map) shopMapDTO {
	b := m.Bounds()
	return shopMapDTO{Key: m.Key, Name: m.DisplayName, MinX: b.MinX, MaxX: b.MaxX, MinZ: b.MinZ, MaxZ: b.MaxZ}
}

type shopCoordinatesDTO struct {
	X float64 `json:"x"`
	Z float64 `json:"z"`
}

// shopDeliveryDTO is the player-safe delivery view (never an admin actor or cancel detail).
type shopDeliveryDTO struct {
	ID           int64               `json:"id"`
	PurchaseID   int64               `json:"purchaseId"`
	DeliveryType string              `json:"deliveryType"`
	Policy       string              `json:"policy"`
	Status       string              `json:"status"`
	Map          *shopMapDTO         `json:"map"`
	Coordinates  *shopCoordinatesDTO `json:"coordinates"`
	CreatedAt    string              `json:"createdAt"`
	UpdatedAt    string              `json:"updatedAt"`
	FulfilledAt  *string             `json:"fulfilledAt"`
	CancelledAt  *string             `json:"cancelledAt"`
}

type adminShopDeliveryDTO struct {
	shopDeliveryDTO
	Player                   economyPlayerDTO      `json:"player"`
	GameServerID             *int64                `json:"gameServerId"`
	PurchaseStatus           string                `json:"purchaseStatus,omitempty"`
	Items                    []shopPurchaseItemDTO `json:"items,omitempty"`
	FulfilledByDiscordUserID *string               `json:"fulfilledByDiscordUserId"`
	CancelledByDiscordUserID *string               `json:"cancelledByDiscordUserId"`
	CancelReason             *string               `json:"cancelReason"`
}

func deliveryDTO(d repository.ShopDelivery) shopDeliveryDTO {
	out := shopDeliveryDTO{ID: d.ID, PurchaseID: d.PurchaseID, DeliveryType: d.DeliveryType, Policy: d.Policy, Status: d.Status,
		CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: d.UpdatedAt.UTC().Format(time.RFC3339),
		FulfilledAt: nullableTimeStr(d.FulfilledAt), CancelledAt: nullableTimeStr(d.CancelledAt)}
	if d.MapKey != "" {
		if m, ok := dayzmap.Lookup(d.MapKey); ok {
			md := mapDTO(m)
			out.Map = &md
		} else {
			out.Map = &shopMapDTO{Key: d.MapKey, Name: d.MapKey}
		}
	}
	if d.X != nil && d.Z != nil {
		out.Coordinates = &shopCoordinatesDTO{X: *d.X, Z: *d.Z}
	}
	return out
}

func adminDeliveryDTO(d repository.ShopDelivery) adminShopDeliveryDTO {
	out := adminShopDeliveryDTO{shopDeliveryDTO: deliveryDTO(d), Player: economyPlayerDTO{AccountID: d.PlayerID, Gamertag: d.PlayerName}, GameServerID: d.GameServerID,
		PurchaseStatus: d.PurchaseStatus, FulfilledByDiscordUserID: optStr(d.FulfilledByDiscordID), CancelledByDiscordUserID: optStr(d.CancelledByDiscordID), CancelReason: optStr(d.CancelReason)}
	for _, it := range d.Items {
		out.Items = append(out.Items, shopPurchaseItemDTO{ProductID: it.ProductID, ProductName: it.ProductName, UnitPricePoints: it.UnitPricePoints, Quantity: it.Quantity, LineTotalPoints: it.LineTotalPoints})
	}
	return out
}

func deliveryPolicyOf(p repository.ShopPurchase) string {
	if p.Delivery == nil {
		return ""
	}
	return p.Delivery.Policy
}

type shopDeliverySettingsDTO struct {
	Map                         *shopMapDTO `json:"map"`
	CoordinateDeliveryAvailable bool        `json:"coordinateDeliveryAvailable"`
	AutomaticDelivery           bool        `json:"automaticDelivery"` // always false in Phase 2A
}

type adminShopDeliverySettingsDTO struct {
	shopDeliverySettingsDTO
	MapKey             *string      `json:"mapKey"`
	SupportedMaps      []shopMapDTO `json:"supportedMaps"`
	UpdatedAt          *string      `json:"updatedAt"`
	UpdatedByDiscordID *string      `json:"updatedByDiscordUserId"`
}

func deliverySettingsDTO(s shop.DeliverySettings) shopDeliverySettingsDTO {
	out := shopDeliverySettingsDTO{}
	if s.Map != nil {
		m := mapDTO(*s.Map)
		out.Map, out.CoordinateDeliveryAvailable = &m, true
	}
	return out
}

func adminDeliverySettingsDTO(s shop.DeliverySettings) adminShopDeliverySettingsDTO {
	out := adminShopDeliverySettingsDTO{shopDeliverySettingsDTO: deliverySettingsDTO(s), MapKey: optStr(s.MapKey),
		UpdatedAt: nullableTimeStr(s.Settings.UpdatedAt), UpdatedByDiscordID: optStr(s.Settings.UpdatedByDiscordID)}
	for _, m := range dayzmap.Supported() {
		out.SupportedMaps = append(out.SupportedMaps, mapDTO(m))
	}
	return out
}

// --- handlers ---------------------------------------------------------------------------------------

// handleShopDeliverySettings is GET .../shop/delivery-settings (player): the map and its bounds,
// so the website can render coordinate input.
func (a *App) handleShopDeliverySettings(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopContext(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	s, err := a.Shop.DeliverySettings(ctx, er.scope)
	if err != nil {
		shopFailed(w, "load shop delivery settings", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, struct {
		Settings shopDeliverySettingsDTO `json:"settings"`
	}{deliverySettingsDTO(s)})
}

func (a *App) handleShopAdminDeliverySettings(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopContext(w, r, true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	s, err := a.Shop.DeliverySettings(ctx, er.scope)
	if err != nil {
		shopFailed(w, "load shop delivery settings", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, struct {
		Settings adminShopDeliverySettingsDTO `json:"settings"`
	}{adminDeliverySettingsDTO(s)})
}

type shopDeliverySettingsBody struct {
	MapKey *string `json:"mapKey"` // null or "" clears the map
}

// handleShopSetDeliverySettings is PUT .../shop/delivery-settings (OWNER/ADMIN): sets the
// installation's map. Only maps with verified bounds are accepted.
func (a *App) handleShopSetDeliverySettings(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasShopAdminLimiter, rateLimitKey(r)) {
		return
	}
	var body shopDeliverySettingsBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	key := ""
	if body.MapKey != nil {
		key = *body.MapKey
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	s, err := a.Shop.SetDeliveryMap(ctx, er.scope, er.user.ID, key)
	if err != nil {
		shopFailed(w, "update shop delivery settings", err)
		return
	}
	shopAudit("shop_delivery_settings_updated", er, "map_key", s.MapKey)
	writeSaaSJSON(w, http.StatusOK, struct {
		Settings adminShopDeliverySettingsDTO `json:"settings"`
	}{adminDeliverySettingsDTO(s)})
}

// handleShopAdminDeliveries is GET .../shop/admin/deliveries?status=&policy=&playerId=&limit=&cursor=
// (OWNER/ADMIN): the delivery queue, newest first.
func (a *App) handleShopAdminDeliveries(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopContext(w, r, true)
	if !ok {
		return
	}
	q, ok := shopQuery(w, r)
	if !ok {
		return
	}
	limit, ok := factionLimit(w, r, shop.DefaultLimit, shop.MaxLimit)
	if !ok {
		return
	}
	list := shop.DeliveryList{Status: q.Get("status"), Policy: q.Get("policy"), Limit: limit, Cursor: strings.TrimSpace(q.Get("cursor"))}
	if list.PlayerID, ok = optionalID(w, q, "playerId"); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	page, err := a.Shop.AdminDeliveries(ctx, er.scope, list)
	if err != nil {
		shopFailed(w, "list shop deliveries", err)
		return
	}
	out := shopPage[adminShopDeliveryDTO]{Currency: economy.ChampionPoints, Items: make([]adminShopDeliveryDTO, 0, len(page.Items)), NextCursor: optStr(page.NextCursor), Limit: page.Limit}
	for _, d := range page.Items {
		out.Items = append(out.Items, adminDeliveryDTO(d))
	}
	writeSaaSJSON(w, http.StatusOK, out)
}

func (a *App) handleShopAdminDelivery(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopContext(w, r, true)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "deliveryID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	d, err := a.Shop.AdminDelivery(ctx, er.scope, id)
	if err != nil {
		shopFailed(w, "load shop delivery", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, struct {
		Delivery adminShopDeliveryDTO `json:"delivery"`
	}{adminDeliveryDTO(*d)})
}

// handleShopMyDelivery is GET .../shop/me/deliveries/{deliveryID} (player): only the acting user's
// own delivery; anyone else's is 404 DELIVERY_NOT_FOUND.
func (a *App) handleShopMyDelivery(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopContext(w, r, false)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "deliveryID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	d, err := a.Shop.MyDelivery(ctx, er.scope, er.user.DiscordUserID, id)
	if err != nil {
		shopFailed(w, "load shop delivery", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, struct {
		Delivery shopDeliveryDTO `json:"delivery"`
	}{deliveryDTO(*d)})
}
