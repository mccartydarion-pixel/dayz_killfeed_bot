package stadium

import (
	"fmt"
	"math"
	"sort"

	"github.com/yourname/dayz-killfeed/internal/dayzmap"
)

// Object is one entry of a DayZ object-spawner file, field for field as ITEM_SpawnerObject in
// Bohemia's scripts/3_game/objectspawner.c (the same shape the Champion Shop writes). pos is
// [x, y (altitude), z]; ypr is [yaw, pitch, roll] in degrees, where yaw is the compass bearing of
// the model's forward axis, clockwise from north (verified against community spawner files: rows
// of containers run along bearing yaw, rows of castle walls along bearing yaw+90).
type Object struct {
	Name                string     `json:"name"`
	Pos                 [3]float64 `json:"pos"`
	Ypr                 [3]float64 `json:"ypr"`
	Scale               float64    `json:"scale"`
	EnableCEPersistency bool       `json:"enableCEPersistency"`
	CustomString        string     `json:"customString"`
}

// CustomStringPrefix tags every stadium object; the index follows.
const CustomStringPrefix = "champion:stadium:v1:"

// Zone kinds of the preview.
const (
	ZoneArena             = "ARENA"
	ZoneGateRed           = "GATE_RED"
	ZoneGateBlue          = "GATE_BLUE"
	ZoneWingRed           = "WING_RED"
	ZoneWingBlue          = "WING_BLUE"
	ZoneLockerRoomRed     = "LOCKER_ROOM_RED"
	ZoneClothingRoomRed   = "CLOTHING_ROOM_RED"
	ZoneLockerRoomBlue    = "LOCKER_ROOM_BLUE"
	ZoneClothingRoomBlue  = "CLOTHING_ROOM_BLUE"
	ZoneCommentary        = "COMMENTARY"
	ZoneStandNorthWest    = "STAND_NW"
	ZoneStandNorthEast    = "STAND_NE"
	ZoneStandSouthWest    = "STAND_SW"
	ZoneStandSouthEast    = "STAND_SE"
	RoomLockerRoom        = "LOCKER_ROOM"
	RoomClothingRoom      = "CLOTHING_ROOM"
	sideRed, sideBlue     = "RED", "BLUE"
	gateOpeningHalf       = 4.47 // metres: the gap between the two gate containers is 8.94 m
	containerLength       = 6.06
	containerWidth        = 2.44
	wallLength            = 20.0
	tunnelGap             = 0.3
	tunnelStart           = 2.0 // metres outside the gate line
	itemSpacing           = 1.1
	itemColumn            = 0.6
	itemMargin            = 0.7 // from the tunnel's ends (22 units fit at the 1.1 m pitch)
	footprintMargin       = 3.0
	maxOvershootTolerance = 1.0 // a wall may overlap the previous piece by this much
)

// Zone is a labelled rectangle of the site in map coordinates (after rotation).
type Zone struct {
	Kind    string       `json:"kind"`
	Name    string       `json:"name"`
	Polygon [][2]float64 `json:"polygon"`
	Center  [2]float64   `json:"center"`
}

// ClassCount is how many objects of one class the layout holds.
type ClassCount struct {
	ClassName string `json:"className"`
	Count     int    `json:"count"`
}

// ItemCount is how many items of one class spawn in the rooms of one kind (both wings together).
type ItemCount struct {
	ClassName string `json:"className"`
	Count     int    `json:"count"`
	Room      string `json:"room"`
}

// Preview is what the website draws and lists before a build.
type Preview struct {
	ObjectCount int          `json:"objectCount"`
	Footprint   [][2]float64 `json:"footprint"`
	Zones       []Zone       `json:"zones"`
	Classes     []ClassCount `json:"classes"`
	Items       []ItemCount  `json:"items"`
	Warnings    []string     `json:"warnings"`
}

// Layout is the built stadium: the spawner objects in file order and the preview.
type Layout struct {
	Objects []Object
	Preview Preview
}

// size is the inner arena rectangle.
type size struct{ L, W float64 }

var sizes = map[string]size{SizeSmall: {60, 40}, SizeMedium: {80, 60}, SizeLarge: {100, 80}}

// kind decides which altitude offset a piece gets.
type kind int

const (
	kindWall kind = iota
	kindContainer
	kindItem
	kindTower
	kindMisc
)

// piece is one object in the local frame: u east, v north, metres from the centre; heading is the
// local yaw (compass, clockwise from local north).
type piece struct {
	name    string
	u, v    float64
	heading float64
	kind    kind
	room    string // items only
}

type builder struct {
	p        Params
	sz       size
	pieces   []piece
	zones    []localZone
	warnings []string
}

type localZone struct {
	kind, name             string
	minU, maxU, minV, maxV float64
}

func (b *builder) add(name string, u, v, heading float64, k kind) {
	if !StructureClasses[name] {
		panic("stadium: " + name + " is not an allowlisted structure")
	}
	b.pieces = append(b.pieces, piece{name: name, u: u, v: v, heading: heading, kind: k})
}

func (b *builder) addItem(name string, u, v float64, room string) {
	b.pieces = append(b.pieces, piece{name: name, u: u, v: v, kind: kindItem, room: room})
}

func (b *builder) zone(kind, name string, minU, maxU, minV, maxV float64) {
	b.zones = append(b.zones, localZone{kind, name, minU, maxU, minV, maxV})
}

func (b *builder) warn(format string, args ...any) {
	b.warnings = append(b.warnings, fmt.Sprintf(format, args...))
}

// Build turns validated Params into the layout. It is deterministic: the same Params always give
// byte-identical objects. It refuses a layout with more than MaxObjects objects, any object
// outside the map, or an altitude outside the sanity range.
func Build(p Params) (Layout, error) {
	if err := p.Validate(); err != nil {
		return Layout{}, err
	}
	m, _ := dayzmap.Lookup(p.Map())
	b := &builder{p: p, sz: sizes[p.Size]}
	if p.MapKey == "" {
		b.warn("no map is configured for this server: the site was checked against %s bounds", m.DisplayName)
	}
	b.warnUnverified()
	b.walls()
	b.gatesAndWings()
	if p.Extras.MapPillar {
		b.mapPillar()
	}
	b.cover()
	if p.Extras.Stairs {
		b.stands()
	}
	if p.Extras.CommentaryTower {
		b.tower()
	}
	if p.Extras.Flags {
		b.flags()
	}
	if len(b.pieces) > MaxObjects {
		return Layout{}, invalid("the layout has %d objects; at most %d are allowed", len(b.pieces), MaxObjects)
	}

	objs := make([]Object, 0, len(b.pieces))
	minU, maxU, minV, maxV := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	for i, pc := range b.pieces {
		x, z := b.world(pc.u, pc.v)
		y := p.AltitudeY + b.offset(pc.kind)
		if !m.Contains(x, z) {
			return Layout{}, invalid("the %s at (%.1f, %.1f) would be outside %s: move the centre or turn the arena", pc.name, x, z, m.DisplayName)
		}
		if y < MinAltitudeY || y > MaxAltitudeY {
			return Layout{}, invalid("the %s would be at altitude %.2f, outside the sane range", pc.name, y)
		}
		minU, maxU, minV, maxV = math.Min(minU, pc.u), math.Max(maxU, pc.u), math.Min(minV, pc.v), math.Max(maxV, pc.v)
		objs = append(objs, Object{
			Name:         pc.name,
			Pos:          [3]float64{round3(x), round3(y), round3(z)},
			Ypr:          [3]float64{roundYaw(pc.heading + p.YawDeg), 0, 0},
			Scale:        1,
			CustomString: fmt.Sprintf("%s%d", CustomStringPrefix, i),
		})
	}

	pv := Preview{ObjectCount: len(objs), Footprint: b.polygon(minU-footprintMargin, maxU+footprintMargin, minV-footprintMargin, maxV+footprintMargin), Warnings: b.warnings}
	if pv.Warnings == nil {
		pv.Warnings = []string{}
	}
	pv.Zones = make([]Zone, 0, len(b.zones))
	for _, z := range b.zones {
		cx, cz := b.world((z.minU+z.maxU)/2, (z.minV+z.maxV)/2)
		pv.Zones = append(pv.Zones, Zone{Kind: z.kind, Name: z.name, Polygon: b.polygon(z.minU, z.maxU, z.minV, z.maxV), Center: [2]float64{round3(cx), round3(cz)}})
	}
	classes := map[string]int{}
	items := map[[2]string]int{}
	for _, pc := range b.pieces {
		if pc.kind == kindItem {
			items[[2]string{pc.room, pc.name}]++
			continue
		}
		classes[pc.name]++
	}
	pv.Classes = make([]ClassCount, 0, len(classes))
	for name, n := range classes {
		pv.Classes = append(pv.Classes, ClassCount{name, n})
	}
	sort.Slice(pv.Classes, func(i, j int) bool { return pv.Classes[i].ClassName < pv.Classes[j].ClassName })
	pv.Items = make([]ItemCount, 0, len(items))
	for key, n := range items {
		pv.Items = append(pv.Items, ItemCount{ClassName: key[1], Count: n, Room: key[0]})
	}
	sort.Slice(pv.Items, func(i, j int) bool {
		if pv.Items[i].Room != pv.Items[j].Room {
			return pv.Items[i].Room < pv.Items[j].Room
		}
		return pv.Items[i].ClassName < pv.Items[j].ClassName
	})
	return Layout{Objects: objs, Preview: pv}, nil
}

// world rotates a local point clockwise by YawDeg about the centre and translates it: a positive
// yaw turns local east towards south (the compass sense), so at yaw 90 the west gate is north.
func (b *builder) world(u, v float64) (x, z float64) {
	t := b.p.YawDeg * math.Pi / 180
	s, c := math.Sin(t), math.Cos(t)
	return b.p.CenterX + u*c + v*s, b.p.CenterZ - u*s + v*c
}

func (b *builder) polygon(minU, maxU, minV, maxV float64) [][2]float64 {
	out := make([][2]float64, 0, 4)
	for _, uv := range [][2]float64{{minU, minV}, {maxU, minV}, {maxU, maxV}, {minU, maxV}} {
		x, z := b.world(uv[0], uv[1])
		out = append(out, [2]float64{round3(x), round3(z)})
	}
	return out
}

func (b *builder) offset(k kind) float64 {
	o := b.p.Offsets
	switch k {
	case kindWall:
		return o.WallY
	case kindContainer:
		return o.ContainerY
	case kindItem:
		return o.ItemY
	case kindTower:
		return o.TowerY
	}
	return o.MiscY
}

// walls rings the arena. North and south sides: 20 m segments from corner to corner (yaw 0). East
// and west sides: the gate opening (20 m, centred on v = 0) is left open and narrowed to 8.94 m by
// a closed container on each side of it; what is left between the container and the corner is
// filled with more containers and 20 m wall segments (sideFill), so every size closes cleanly.
func (b *builder) walls() {
	L, W := b.sz.L, b.sz.W
	for i := 0; i < int(L/wallLength); i++ {
		u := -L/2 + wallLength/2 + float64(i)*wallLength
		b.add(ClassWall, u, W/2, 0, kindWall)
		b.add(ClassWall, u, -W/2, 0, kindWall)
	}
	containers, walls := sideFill(W)
	for _, su := range []float64{-1, 1} {
		for _, sv := range []float64{1, -1} {
			for _, v := range containers {
				b.add(ClassContainerGate, su*L/2, sv*v, 0, kindContainer)
			}
			for _, v := range walls {
				b.add(ClassWall, su*L/2, sv*v, 90, kindWall)
			}
		}
	}
	b.zone(ZoneArena, "Arena", -L/2, L/2, -W/2, W/2)
	b.zone(ZoneGateRed, "Red gate", -L/2-1.5, -L/2+1.5, -gateOpeningHalf, gateOpeningHalf)
	b.zone(ZoneGateBlue, "Blue gate", L/2-1.5, L/2+1.5, -gateOpeningHalf, gateOpeningHalf)
}

// sideFill is one quarter of an east or west side (v > 0): the centres of the closed containers
// and of the wall segments between the gate opening and the corner at v = W/2. The first
// container always sits at v = 7.5 (the gate narrowing). Then, from its outer end to the corner:
// as many 20 m walls as fit (a wall may overlap the piece before it by up to 1 m) and, when the
// rest is shorter than a wall, more containers placed before the walls - so the gate is flanked
// by a pillar of containers and the corner is a wall. The last piece may overshoot the corner by
// less than one container (2.65 m on SMALL and LARGE); a stand never sits inside that overshoot.
func sideFill(W float64) (containers, walls []float64) {
	containers = []float64{7.5}
	end := 7.5 + containerLength/2
	rem := W/2 - end
	nWalls := int(math.Floor((rem + maxOvershootTolerance) / wallLength))
	left := rem - float64(nWalls)*wallLength
	nCont := 0
	if left > 0 {
		nCont = int(math.Ceil(left / containerLength))
	}
	for i := 0; i < nCont; i++ {
		containers = append(containers, end+containerLength/2+float64(i)*containerLength)
	}
	end += float64(nCont) * containerLength
	for i := 0; i < nWalls; i++ {
		if nCont == 0 {
			// Corner-aligned, like the north and south walls.
			walls = append(walls, W/2-wallLength/2-float64(nWalls-1-i)*wallLength)
		} else {
			walls = append(walls, end+wallLength/2+float64(i)*wallLength)
		}
	}
	return containers, walls
}

// SideOvershoot is how far (metres) the east/west side pieces of a size reach beyond the north
// and south wall lines (0 when the side ends exactly at the corner).
func SideOvershoot(sizeName string) float64 {
	sz, ok := sizes[sizeName]
	if !ok {
		return 0
	}
	containers, walls := sideFill(sz.W)
	end := 0.0
	for _, v := range containers {
		end = math.Max(end, v+containerLength/2)
	}
	for _, v := range walls {
		end = math.Max(end, v+wallLength/2)
	}
	return math.Round((end-sz.W/2)*100) / 100
}

// gatesAndWings builds, outside each gate, two walk-through container tunnels side by side (the
// gun locker room at v = +3.4 and the clothing room at v = -3.4), a closed container as a side
// wall at v = +-7.5, and the kit items on the tunnel floors.
func (b *builder) gatesAndWings() {
	L := b.sz.L
	span := 2*containerLength + tunnelGap // 12.42 m
	for _, side := range []struct {
		su   float64
		name string
	}{{-1, sideRed}, {1, sideBlue}} {
		near := L/2 + tunnelStart
		far := near + span
		first := near + containerLength/2
		second := near + containerLength + tunnelGap + containerLength/2
		for _, v := range []float64{3.4, -3.4} {
			b.add(ClassContainerOpen, side.su*first, v, 90, kindContainer)
			b.add(ClassContainerOpen, side.su*second, v, 90, kindContainer)
		}
		mid := (near + far) / 2
		b.add(ClassContainerSide, side.su*mid, 7.5, 90, kindContainer)
		b.add(ClassContainerSide, side.su*mid, -7.5, 90, kindContainer)

		lockerZone, clothingZone, wingZone := ZoneLockerRoomRed, ZoneClothingRoomRed, ZoneWingRed
		label := "Red"
		if side.name == sideBlue {
			lockerZone, clothingZone, wingZone = ZoneLockerRoomBlue, ZoneClothingRoomBlue, ZoneWingBlue
			label = "Blue"
		}
		lo, hi := math.Min(side.su*near, side.su*far), math.Max(side.su*near, side.su*far)
		b.zone(wingZone, label+" wing", lo, hi, -7.5-containerWidth/2, 7.5+containerWidth/2)
		b.zone(lockerZone, label+" gun locker room", lo, hi, 3.4-containerWidth/2, 3.4+containerWidth/2)
		b.zone(clothingZone, label+" clothing room", lo, hi, -3.4-containerWidth/2, -3.4+containerWidth/2)

		b.placeItems(side.su, near, far, 3.4, RoomLockerRoom, lockerItems(b.p))
		b.placeItems(side.su, near, far, -3.4, RoomClothingRoom, clothingItems(b.p, side.name))
	}
}

// placeItems lays the units of a kit on a tunnel floor: two columns (v +-0.6), one unit per
// column per step, from the gate end outwards, 1.1 m apart. More units than fit at that pitch
// are packed closer, down to what the tunnel holds (the custom limits keep that above 0.5 m).
func (b *builder) placeItems(su, near, far, tunnelV float64, room string, units []string) {
	if len(units) == 0 {
		return
	}
	usable := (far - near) - 2*itemMargin
	steps := (len(units) + 1) / 2
	spacing := itemSpacing
	if steps > 1 && float64(steps-1)*spacing > usable {
		spacing = usable / float64(steps-1)
		b.warn("%d items in the %s are packed %.2f m apart instead of %.1f m to fit the tunnel", len(units), roomLabel(room), spacing, itemSpacing)
	}
	for i, name := range units {
		u := near + itemMargin + float64(i/2)*spacing
		v := tunnelV + itemColumn
		if i%2 == 1 {
			v = tunnelV - itemColumn
		}
		b.addItem(name, su*u, v, room)
	}
}

func roomLabel(room string) string {
	if room == RoomLockerRoom {
		return "gun locker room"
	}
	return "clothing room"
}

// kit lists expand to one entry per unit, in listing order.
func expand(items []Item) []string {
	var out []string
	for _, it := range items {
		for i := 0; i < it.Count; i++ {
			out = append(out, it.ClassName)
		}
	}
	return out
}

// Kits, as docs/STADIUM.md lists them.
var (
	kitM4Only    = []Item{{"M4A1", 2}, {"Mag_STANAG_30Rnd", 4}, {"AmmoBox_556x45_20Rnd", 2}, {"M4_T3NRDSOptic", 2}, {"M4_RISHndgrd", 2}, {"M4_MPBttstck", 2}, {"BandageDressing", 2}, {"Morphine", 2}}
	kitSidearm   = []Item{{"FNX45", 2}, {"Mag_FNX45_15Rnd", 2}}
	kitClothRed  = []Item{{"TrackSuitJacket_Red", 2}, {"TrackSuitPants_Red", 2}}
	kitClothBlue = []Item{{"TrackSuitJacket_Blue", 2}, {"TrackSuitPants_Blue", 2}}
	kitClothBoth = []Item{{"BallisticHelmet_Black", 2}, {"PlateCarrierVest", 2}, {"TacticalGloves_Black", 2}, {"CombatBoots_Black", 2}}
	kitTactical  = []Item{{"TTSKOJacket_Camo", 2}, {"TTSKOPants", 2}, {"BallisticHelmet_Green", 2}, {"PlateCarrierVest", 2}, {"TacticalGloves_Black", 2}, {"CombatBoots_Black", 2}}
)

// LockerKitItems is the item list of a locker kit (per tunnel).
func LockerKitItems(p Params) []Item {
	switch p.LockerKit {
	case LockerM4PlusSidearm:
		return append(append([]Item{}, kitM4Only...), kitSidearm...)
	case LockerCustom:
		return append([]Item{}, p.CustomLockerItems...)
	}
	return append([]Item{}, kitM4Only...)
}

// ClothingKitItems is the item list of a clothing kit for one wing ("RED" or "BLUE").
func ClothingKitItems(p Params, side string) []Item {
	switch p.ClothingKit {
	case ClothingTactical:
		return append([]Item{}, kitTactical...)
	case ClothingCustom:
		return append([]Item{}, p.CustomClothingItems...)
	}
	corner := kitClothRed
	if side == sideBlue {
		corner = kitClothBlue
	}
	return append(append([]Item{}, corner...), kitClothBoth...)
}

func lockerItems(p Params) []string                { return expand(LockerKitItems(p)) }
func clothingItems(p Params, side string) []string { return expand(ClothingKitItems(p, side)) }

// warnUnverified reports every custom item class that is not in VerifiedItems: the name is well
// formed, but nobody has seen the spawner create it, so a typo would simply spawn nothing.
func (b *builder) warnUnverified() {
	seen := map[string]bool{}
	for _, items := range [][]Item{LockerKitItems(b.p), ClothingKitItems(b.p, sideRed), ClothingKitItems(b.p, sideBlue)} {
		for _, it := range items {
			if !VerifiedItems[it.ClassName] && !seen[it.ClassName] {
				seen[it.ClassName] = true
				b.warn("%s is not in Champion's list of verified item classes: check the spelling against the game's types.xml", it.ClassName)
			}
		}
	}
}

// mapPillar is the "map in the middle": four wall maps back to back at the centre, each facing
// outwards.
func (b *builder) mapPillar() {
	b.add(ClassMap, 0.35, 0, 90, kindMisc)
	b.add(ClassMap, -0.35, 0, 270, kindMisc)
	b.add(ClassMap, 0, 0.35, 0, kindMisc)
	b.add(ClassMap, 0, -0.35, 180, kindMisc)
}

// cover is symmetric about both axes. LIGHT: four crate clusters (three crates in a 1.5 m
// triangle, the triangle pointing away from the centre line) and two long block rows across the
// north and south halves. HEAVY adds a wrecked UAZ and a block row on each side of the centre.
func (b *builder) cover() {
	if b.p.Cover == CoverNone {
		return
	}
	L, W := b.sz.L, b.sz.W
	for _, su := range []float64{-1, 1} {
		for _, sv := range []float64{1, -1} {
			cu, cv := su*L/6, sv*W/5
			b.add(ClassCrate, cu, cv+sv*0.866, 0, kindMisc)
			b.add(ClassCrate, cu-0.75, cv-sv*0.433, 0, kindMisc)
			b.add(ClassCrate, cu+0.75, cv-sv*0.433, 0, kindMisc)
		}
	}
	b.add(ClassBlocks, 0, W/3, 0, kindMisc)
	b.add(ClassBlocks, 0, -W/3, 0, kindMisc)
	if b.p.Cover == CoverHeavy {
		b.add(ClassWreck, -L/4, 0, 0, kindMisc)
		b.add(ClassWreck, L/4, 0, 0, kindMisc)
		b.add(ClassBlocks, -L/3, 0, 90, kindMisc)
		b.add(ClassBlocks, L/3, 0, 90, kindMisc)
	}
}

// Stand placement: outside each corner, 5 m past the east/west wall line and 3 m past the north/
// south wall line (the 5 keeps a stand clear of the side pieces that overshoot the corner on
// SMALL and LARGE). The stairs face the long (north or south) wall - a guess at which way the
// model's "front" points; turn them with the wall if they come out backwards.
const (
	standOutU = 5.0
	standOutV = 3.0
	standSize = 6.0 // the footprint the preview reserves for a stand (a guess)
)

func (b *builder) stands() {
	L, W := b.sz.L, b.sz.W
	for _, c := range []struct {
		su, sv  float64
		kind    string
		name    string
		heading float64
	}{
		{-1, 1, ZoneStandNorthWest, "North-west stand", 180},
		{1, 1, ZoneStandNorthEast, "North-east stand", 180},
		{-1, -1, ZoneStandSouthWest, "South-west stand", 0},
		{1, -1, ZoneStandSouthEast, "South-east stand", 0},
	} {
		u, v := c.su*(L/2+standOutU), c.sv*(W/2+standOutV)
		b.add(ClassStairs, u, v, c.heading, kindMisc)
		b.zone(c.kind, c.name, u-standSize/2, u+standSize/2, v-standSize/2, v+standSize/2)
	}
}

// tower is the commentary tower north of the arena, facing it.
func (b *builder) tower() {
	W := b.sz.W
	b.add(ClassTower, 0, W/2+12, 180, kindTower)
	b.zone(ZoneCommentary, "Commentary tower", -4, 4, W/2+8, W/2+16)
}

func (b *builder) flags() {
	L := b.sz.L
	for _, su := range []float64{-1, 1} {
		for _, sv := range []float64{1, -1} {
			b.add(ClassFlag, su*(L/2+17), sv*9, 0, kindMisc)
		}
	}
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

func roundYaw(deg float64) float64 {
	deg = math.Mod(deg, 360)
	if deg < 0 {
		deg += 360
	}
	r := math.Round(deg*100) / 100
	if r >= 360 {
		r = 0
	}
	return r
}
