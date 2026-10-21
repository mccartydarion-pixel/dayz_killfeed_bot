// Package stadium builds the "Stadium": a tournament arena made of DayZ map objects, written as a
// cfggameplay.json object-spawner file into the server's custom/ folder (docs/STADIUM.md).
//
// This package is pure: Build turns Params into the exact list of spawner objects and a preview
// for the website, deterministically, with no I/O. The spawner creates every object once per
// server start at the exact [x, y, z] given - there is no ground snapping - so the altitude must
// come from a real recorded position (the owner's own ADM position) and the site must be flat.
package stadium

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/dayzmap"
)

// Sizes: the inner arena rectangle, L (east-west) x W (north-south) in metres.
const (
	SizeSmall  = "SMALL"  // 60 x 40
	SizeMedium = "MEDIUM" // 80 x 60 (default)
	SizeLarge  = "LARGE"  // 100 x 80
)

// Cover inside the arena.
const (
	CoverNone  = "NONE"
	CoverLight = "LIGHT" // default
	CoverHeavy = "HEAVY"
)

// Locker-room kits (one set per tunnel).
const (
	LockerM4Only        = "M4_ONLY" // default
	LockerM4PlusSidearm = "M4_PLUS_SIDEARM"
	LockerCustom        = "CUSTOM"
)

// Clothing-room kits (one set per wing).
const (
	ClothingRedBlueCorners = "RED_BLUE_CORNERS" // default
	ClothingTactical       = "TACTICAL"
	ClothingCustom         = "CUSTOM"
)

// Altitude sanity range: the same range the Shop applies to a delivery point
// (nitradodelivery.MinAltitude / MaxAltitude; TestAltitudeRangeMatchesShop pins it). The value
// itself must come from a recorded position; Champion never invents one.
const (
	MinAltitudeY = -50.0
	MaxAltitudeY = 1500.0
)

// Limits.
const (
	MaxObjects       = 500 // the whole file
	MaxCustomEntries = 40  // entries per custom list
	MaxCustomUnits   = 40  // items per room from a custom list (what fits in a tunnel)
	MaxCustomCount   = 40  // units of one entry
	MaxOffset        = 5.0 // metres, each per-kind altitude offset
	DefaultItemY     = 0.05
	DefaultMapKey    = "chernarusplus"
)

// Item is one class name with a count, as the custom kits take it.
type Item struct {
	ClassName string `json:"className"`
	Count     int    `json:"count"`
}

// Extras switches the optional pieces. Omitted in a request means on.
type Extras struct {
	Stairs          bool `json:"stairs"`
	CommentaryTower bool `json:"commentaryTower"`
	Flags           bool `json:"flags"`
	MapPillar       bool `json:"mapPillar"`
}

// Offsets are per-kind altitude offsets in metres added to AltitudeY: the object kinds sit on
// different model origins, and the owner can nudge each kind after seeing the first build.
type Offsets struct {
	WallY      float64 `json:"wallY"`
	ContainerY float64 `json:"containerY"`
	ItemY      float64 `json:"itemY"`
	TowerY     float64 `json:"towerY"`
	MiscY      float64 `json:"miscY"`
}

// Params is the owner's stadium configuration (docs/STADIUM.md "Params"). Parse fills the
// defaults and Validate checks it; a stored Params is always complete.
type Params struct {
	// MapKey names the terrain the site is checked against (internal/dayzmap). Empty is filled by
	// the caller from the installation's configured map; the layout then uses Chernarus bounds and
	// warns that the map was assumed.
	MapKey    string  `json:"mapKey"`
	CenterX   float64 `json:"centerX"`
	CenterZ   float64 `json:"centerZ"`
	AltitudeY float64 `json:"altitudeY"`
	// AltitudeSource records where AltitudeY came from: "ADM:<player>@<RFC3339>" or "MANUAL".
	AltitudeSource      string  `json:"altitudeSource"`
	YawDeg              float64 `json:"yawDeg"`
	Size                string  `json:"size"`
	Cover               string  `json:"cover"`
	LockerKit           string  `json:"lockerKit"`
	CustomLockerItems   []Item  `json:"customLockerItems"`
	ClothingKit         string  `json:"clothingKit"`
	CustomClothingItems []Item  `json:"customClothingItems"`
	Extras              Extras  `json:"extras"`
	Offsets             Offsets `json:"offsets"`
}

// wire is Params as a request carries it: every optional field may be absent.
type wire struct {
	MapKey              *string  `json:"mapKey"`
	CenterX             *float64 `json:"centerX"`
	CenterZ             *float64 `json:"centerZ"`
	AltitudeY           *float64 `json:"altitudeY"`
	AltitudeSource      *string  `json:"altitudeSource"`
	YawDeg              *float64 `json:"yawDeg"`
	Size                *string  `json:"size"`
	Cover               *string  `json:"cover"`
	LockerKit           *string  `json:"lockerKit"`
	CustomLockerItems   []Item   `json:"customLockerItems"`
	ClothingKit         *string  `json:"clothingKit"`
	CustomClothingItems []Item   `json:"customClothingItems"`
	Extras              *struct {
		Stairs          *bool `json:"stairs"`
		CommentaryTower *bool `json:"commentaryTower"`
		Flags           *bool `json:"flags"`
		MapPillar       *bool `json:"mapPillar"`
	} `json:"extras"`
	Offsets *struct {
		WallY      *float64 `json:"wallY"`
		ContainerY *float64 `json:"containerY"`
		ItemY      *float64 `json:"itemY"`
		TowerY     *float64 `json:"towerY"`
		MiscY      *float64 `json:"miscY"`
	} `json:"offsets"`
}

// Defaults is a complete Params with every default and no position (the caller sets the centre
// and altitude).
func Defaults() Params {
	return Params{AltitudeSource: "MANUAL", Size: SizeMedium, Cover: CoverLight, LockerKit: LockerM4Only, ClothingKit: ClothingRedBlueCorners,
		Extras: Extras{Stairs: true, CommentaryTower: true, Flags: true, MapPillar: true}, Offsets: Offsets{ItemY: DefaultItemY}}
}

// ErrInvalidParams wraps every validation failure; its message is written for the owner.
var ErrInvalidParams = errors.New("invalid stadium parameters")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidParams, fmt.Sprintf(format, args...))
}

// Parse decodes a request body into a complete Params: absent fields take their defaults, unknown
// fields are refused, and the result is validated.
func Parse(raw []byte) (Params, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var w wire
	if err := dec.Decode(&w); err != nil {
		return Params{}, invalid("the request body is not a stadium configuration (%v)", err)
	}
	p := Defaults()
	set := func(dst *float64, src *float64) {
		if src != nil {
			*dst = *src
		}
	}
	sets := func(dst *string, src *string) {
		if src != nil {
			*dst = strings.TrimSpace(*src)
		}
	}
	sets(&p.MapKey, w.MapKey)
	set(&p.CenterX, w.CenterX)
	set(&p.CenterZ, w.CenterZ)
	set(&p.AltitudeY, w.AltitudeY)
	sets(&p.AltitudeSource, w.AltitudeSource)
	set(&p.YawDeg, w.YawDeg)
	sets(&p.Size, w.Size)
	sets(&p.Cover, w.Cover)
	sets(&p.LockerKit, w.LockerKit)
	sets(&p.ClothingKit, w.ClothingKit)
	p.CustomLockerItems = append([]Item{}, w.CustomLockerItems...)
	p.CustomClothingItems = append([]Item{}, w.CustomClothingItems...)
	if w.Extras != nil {
		setb := func(dst *bool, src *bool) {
			if src != nil {
				*dst = *src
			}
		}
		setb(&p.Extras.Stairs, w.Extras.Stairs)
		setb(&p.Extras.CommentaryTower, w.Extras.CommentaryTower)
		setb(&p.Extras.Flags, w.Extras.Flags)
		setb(&p.Extras.MapPillar, w.Extras.MapPillar)
	}
	if w.Offsets != nil {
		set(&p.Offsets.WallY, w.Offsets.WallY)
		set(&p.Offsets.ContainerY, w.Offsets.ContainerY)
		set(&p.Offsets.ItemY, w.Offsets.ItemY)
		set(&p.Offsets.TowerY, w.Offsets.TowerY)
		set(&p.Offsets.MiscY, w.Offsets.MiscY)
	}
	if w.CenterX == nil || w.CenterZ == nil {
		return Params{}, invalid("centerX and centerZ are required")
	}
	if w.AltitudeY == nil {
		return Params{}, invalid("altitudeY is required: use the position lookup or enter the altitude from the server log")
	}
	p.normalize()
	if err := p.Validate(); err != nil {
		return Params{}, err
	}
	return p, nil
}

// normalize upper-cases the enums and wraps the yaw into [0, 360).
func (p *Params) normalize() {
	p.Size = strings.ToUpper(p.Size)
	p.Cover = strings.ToUpper(p.Cover)
	p.LockerKit = strings.ToUpper(p.LockerKit)
	p.ClothingKit = strings.ToUpper(p.ClothingKit)
	p.MapKey = strings.ToLower(strings.TrimSpace(p.MapKey))
	if finite(p.YawDeg) {
		p.YawDeg = math.Mod(p.YawDeg, 360)
		if p.YawDeg < 0 {
			p.YawDeg += 360
		}
		if p.YawDeg == 360 || math.Abs(p.YawDeg) < 1e-9 {
			p.YawDeg = 0
		}
	}
	if p.AltitudeSource == "" {
		p.AltitudeSource = "MANUAL"
	}
	if p.CustomLockerItems == nil {
		p.CustomLockerItems = []Item{}
	}
	if p.CustomClothingItems == nil {
		p.CustomClothingItems = []Item{}
	}
	for i := range p.CustomLockerItems {
		p.CustomLockerItems[i].ClassName = strings.TrimSpace(p.CustomLockerItems[i].ClassName)
	}
	for i := range p.CustomClothingItems {
		p.CustomClothingItems[i].ClassName = strings.TrimSpace(p.CustomClothingItems[i].ClassName)
	}
}

var altitudeSourceRe = regexp.MustCompile(`^(MANUAL|ADM:[^\x00-\x1f]{1,140})$`)

// Validate checks a complete Params. The error message says what is wrong in plain words.
func (p Params) Validate() error {
	m, ok := dayzmap.Lookup(p.Map())
	if !ok {
		return invalid("mapKey %q is not a supported map", p.MapKey)
	}
	if !finite(p.CenterX) || !finite(p.CenterZ) || !m.Contains(p.CenterX, p.CenterZ) {
		return invalid("the centre must be inside %s (0 to %.0f on both axes)", m.DisplayName, m.SizeMetres)
	}
	if !finite(p.AltitudeY) || p.AltitudeY < MinAltitudeY || p.AltitudeY > MaxAltitudeY {
		return invalid("altitudeY must be between %.0f and %.0f metres", MinAltitudeY, MaxAltitudeY)
	}
	if !altitudeSourceRe.MatchString(p.AltitudeSource) {
		return invalid("altitudeSource must be MANUAL or ADM:<player>@<time>")
	}
	if !finite(p.YawDeg) || p.YawDeg < 0 || p.YawDeg >= 360 {
		return invalid("yawDeg must be a number of degrees")
	}
	if _, ok := sizes[p.Size]; !ok {
		return invalid("size must be SMALL, MEDIUM or LARGE")
	}
	switch p.Cover {
	case CoverNone, CoverLight, CoverHeavy:
	default:
		return invalid("cover must be NONE, LIGHT or HEAVY")
	}
	switch p.LockerKit {
	case LockerM4Only, LockerM4PlusSidearm:
	case LockerCustom:
		if err := validateItems("customLockerItems", p.CustomLockerItems); err != nil {
			return err
		}
	default:
		return invalid("lockerKit must be M4_ONLY, M4_PLUS_SIDEARM or CUSTOM")
	}
	switch p.ClothingKit {
	case ClothingRedBlueCorners, ClothingTactical:
	case ClothingCustom:
		if err := validateItems("customClothingItems", p.CustomClothingItems); err != nil {
			return err
		}
	default:
		return invalid("clothingKit must be RED_BLUE_CORNERS, TACTICAL or CUSTOM")
	}
	for name, v := range map[string]float64{"wallY": p.Offsets.WallY, "containerY": p.Offsets.ContainerY, "itemY": p.Offsets.ItemY, "towerY": p.Offsets.TowerY, "miscY": p.Offsets.MiscY} {
		if !finite(v) || math.Abs(v) > MaxOffset {
			return invalid("offsets.%s must be between -%.0f and %.0f metres", name, MaxOffset, MaxOffset)
		}
	}
	return nil
}

func validateItems(field string, items []Item) error {
	if len(items) == 0 {
		return invalid("%s must list at least one item for a CUSTOM kit", field)
	}
	if len(items) > MaxCustomEntries {
		return invalid("%s has more than %d entries", field, MaxCustomEntries)
	}
	total := 0
	for i, it := range items {
		if !ValidClassName(it.ClassName) {
			return invalid("%s[%d]: %q is not a DayZ class name (letters, digits and underscores only)", field, i, it.ClassName)
		}
		if it.Count < 1 || it.Count > MaxCustomCount {
			return invalid("%s[%d]: count must be 1 to %d", field, i, MaxCustomCount)
		}
		total += it.Count
	}
	if total > MaxCustomUnits {
		return invalid("%s: at most %d items fit in a room (%d listed)", field, MaxCustomUnits, total)
	}
	return nil
}

// Map is the map key the layout is checked against: MapKey, or Chernarus when none is set.
func (p Params) Map() string {
	if p.MapKey == "" {
		return DefaultMapKey
	}
	return p.MapKey
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
