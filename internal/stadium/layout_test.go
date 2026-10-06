package stadium

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

func testParams(size string) Params {
	p := Defaults()
	p.MapKey, p.CenterX, p.CenterZ, p.AltitudeY, p.Size = "chernarusplus", 4618, 10439, 339.2, size
	return p
}

func mustBuild(t *testing.T, p Params) Layout {
	t.Helper()
	l, err := Build(p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return l
}

// local returns an object's position in the arena frame for a yaw-0 build.
func local(p Params, o Object) (u, v float64) {
	return math.Round((o.Pos[0]-p.CenterX)*100) / 100, math.Round((o.Pos[2]-p.CenterZ)*100) / 100
}

func TestBuildIsDeterministic(t *testing.T) {
	p := testParams(SizeMedium)
	p.YawDeg, p.Cover, p.LockerKit = 123.4, CoverHeavy, LockerM4PlusSidearm
	a, b := mustBuild(t, p), mustBuild(t, p)
	if !bytes.Equal(Render(a.Objects), Render(b.Objects)) {
		t.Fatal("two builds of the same params differ")
	}
	pa, _ := json.Marshal(a.Preview)
	pb, _ := json.Marshal(b.Preview)
	if !bytes.Equal(pa, pb) {
		t.Fatal("two previews of the same params differ")
	}
	for i, o := range a.Objects {
		if o.CustomString != fmt.Sprintf("champion:stadium:v1:%d", i) {
			t.Fatalf("object %d is tagged %q", i, o.CustomString)
		}
		if o.Scale != 1 || o.EnableCEPersistency || o.Ypr[1] != 0 || o.Ypr[2] != 0 {
			t.Fatalf("object %d has unexpected scale/persistency/pitch/roll: %+v", i, o)
		}
	}
}

func TestBuildEmitsOnlyAllowlistedClasses(t *testing.T) {
	for _, size := range []string{SizeSmall, SizeMedium, SizeLarge} {
		for _, cover := range []string{CoverNone, CoverLight, CoverHeavy} {
			for _, locker := range []string{LockerM4Only, LockerM4PlusSidearm} {
				for _, clothing := range []string{ClothingRedBlueCorners, ClothingTactical} {
					for _, extras := range []Extras{{}, {true, true, true, true}} {
						p := testParams(size)
						p.Cover, p.LockerKit, p.ClothingKit, p.Extras = cover, locker, clothing, extras
						l := mustBuild(t, p)
						for _, o := range l.Objects {
							if !Allowed(o.Name) {
								t.Fatalf("%s/%s/%s/%s emits %q, which is not allowlisted", size, cover, locker, clothing, o.Name)
							}
						}
						if len(l.Preview.Warnings) != 0 {
							t.Fatalf("built-in kits warned: %v", l.Preview.Warnings)
						}
					}
				}
			}
		}
	}
}

func TestObjectCountsPerSize(t *testing.T) {
	// Defaults: LIGHT cover, M4_ONLY, RED_BLUE_CORNERS, every extra. The structure counts differ
	// only on the east and west sides (sideFill); the 60 items are the same for every size.
	want := map[string]int{SizeSmall: 117, SizeMedium: 115, SizeLarge: 125}
	for size, n := range want {
		l := mustBuild(t, testParams(size))
		if l.Preview.ObjectCount != n || len(l.Objects) != n {
			t.Fatalf("%s: %d objects, want %d", size, len(l.Objects), n)
		}
		items := 0
		for _, it := range l.Preview.Items {
			items += it.Count
		}
		if items != 60 {
			t.Fatalf("%s: %d items, want 60", size, items)
		}
	}
	// SMALL and LARGE close their sides with container pillars that overshoot the corner by less
	// than half a container; MEDIUM ends exactly at the corner.
	for size, over := range map[string]float64{SizeSmall: 2.65, SizeMedium: 0, SizeLarge: 2.65} {
		if got := SideOvershoot(size); got != over {
			t.Fatalf("%s overshoot %.2f, want %.2f", size, got, over)
		}
	}
	// The class list of the default MEDIUM layout, as docs/STADIUM.md states it.
	l := mustBuild(t, testParams(SizeMedium))
	got := map[string]int{}
	for _, c := range l.Preview.Classes {
		got[c.ClassName] = c.Count
	}
	wantClasses := map[string]int{ClassWall: 12, ClassContainerGate: 4, ClassContainerOpen: 8, ClassContainerSide: 4, ClassMap: 4, ClassCrate: 12, ClassBlocks: 2, ClassStairs: 4, ClassTower: 1, ClassFlag: 4}
	if fmt.Sprint(got) != fmt.Sprint(wantClasses) {
		t.Fatalf("MEDIUM classes %v, want %v", got, wantClasses)
	}
}

// TestMirrorSymmetry: every structure of a yaw-0 build has a twin mirrored about the north-south
// axis (red and blue sides are the same) and, except the commentary tower, about the east-west
// axis.
func TestMirrorSymmetry(t *testing.T) {
	for _, size := range []string{SizeSmall, SizeMedium, SizeLarge} {
		for _, cover := range []string{CoverLight, CoverHeavy} {
			p := testParams(size)
			p.Cover = cover
			l := mustBuild(t, p)
			key := func(name string, u, v float64) string { return fmt.Sprintf("%s@%.2f,%.2f", name, u, v) }
			set := map[string]int{}
			for _, o := range l.Objects {
				if !StructureClasses[o.Name] {
					continue
				}
				u, v := local(p, o)
				set[key(o.Name, u, v)]++
			}
			for _, o := range l.Objects {
				if !StructureClasses[o.Name] {
					continue
				}
				u, v := local(p, o)
				if set[key(o.Name, -u+0, v)] == 0 {
					t.Fatalf("%s/%s: %s at (%.2f, %.2f) has no east-west twin", size, cover, o.Name, u, v)
				}
				if o.Name != ClassTower && set[key(o.Name, u, -v+0)] == 0 {
					t.Fatalf("%s/%s: %s at (%.2f, %.2f) has no north-south twin", size, cover, o.Name, u, v)
				}
			}
			// Items too, by position (the red and blue clothing differ by class only).
			items := map[string]int{}
			for _, o := range l.Objects {
				if StructureClasses[o.Name] {
					continue
				}
				u, v := local(p, o)
				items[key("item", u, v)]++
			}
			for _, o := range l.Objects {
				if StructureClasses[o.Name] {
					continue
				}
				u, v := local(p, o)
				if items[key("item", -u+0, v)] == 0 {
					t.Fatalf("%s: item at (%.2f, %.2f) has no twin in the other wing", size, u, v)
				}
			}
		}
	}
}

func TestRotationMovesTheWestGateNorth(t *testing.T) {
	p := testParams(SizeMedium)
	flat := mustBuild(t, p)
	p.YawDeg = 90
	turned := mustBuild(t, p)
	gate := func(l Layout, kind string) [2]float64 {
		for _, z := range l.Preview.Zones {
			if z.Kind == kind {
				return z.Center
			}
		}
		t.Fatalf("no %s zone", kind)
		return [2]float64{}
	}
	if g := gate(flat, ZoneGateRed); math.Abs(g[0]-(p.CenterX-40)) > 0.01 || math.Abs(g[1]-p.CenterZ) > 0.01 {
		t.Fatalf("yaw 0 red gate at %v", g)
	}
	// Clockwise by 90 degrees: west becomes north (z grows northwards).
	if g := gate(turned, ZoneGateRed); math.Abs(g[0]-p.CenterX) > 0.01 || math.Abs(g[1]-(p.CenterZ+40)) > 0.01 {
		t.Fatalf("yaw 90 red gate at %v, want north of the centre", g)
	}
	if g := gate(turned, ZoneGateBlue); math.Abs(g[0]-p.CenterX) > 0.01 || math.Abs(g[1]-(p.CenterZ-40)) > 0.01 {
		t.Fatalf("yaw 90 blue gate at %v, want south of the centre", g)
	}
	if g := gate(turned, ZoneCommentary); g[0] <= p.CenterX+30 || math.Abs(g[1]-p.CenterZ) > 0.01 {
		t.Fatalf("yaw 90 commentary tower at %v, want east of the centre", g)
	}
	// Every object turned with the site: same order, yaw + 90, same distance from the centre.
	if len(flat.Objects) != len(turned.Objects) {
		t.Fatal("object count changed with the yaw")
	}
	for i := range flat.Objects {
		a, b := flat.Objects[i], turned.Objects[i]
		if a.Name != b.Name || math.Mod(a.Ypr[0]+90, 360) != b.Ypr[0] || a.Pos[1] != b.Pos[1] {
			t.Fatalf("object %d: %+v vs %+v", i, a, b)
		}
		da := math.Hypot(a.Pos[0]-p.CenterX, a.Pos[2]-p.CenterZ)
		db := math.Hypot(b.Pos[0]-p.CenterX, b.Pos[2]-p.CenterZ)
		if math.Abs(da-db) > 0.01 {
			t.Fatalf("object %d moved from %.2f to %.2f metres from the centre", i, da, db)
		}
		// A point rotated clockwise by 90: (u, v) -> (v, -u).
		u, v := a.Pos[0]-p.CenterX, a.Pos[2]-p.CenterZ
		if math.Abs((p.CenterX+v)-b.Pos[0]) > 0.01 || math.Abs((p.CenterZ-u)-b.Pos[2]) > 0.01 {
			t.Fatalf("object %d: (%.2f, %.2f) did not rotate clockwise to (%.2f, %.2f)", i, u, v, b.Pos[0]-p.CenterX, b.Pos[2]-p.CenterZ)
		}
	}
	// A negative yaw is the same site as its positive twin.
	p.YawDeg = -270
	p.normalize()
	if p.YawDeg != 90 {
		t.Fatalf("yaw -270 normalized to %v", p.YawDeg)
	}
}

func TestBuildRefusesSitesOutsideTheMapOrAltitudeRange(t *testing.T) {
	p := testParams(SizeMedium)
	p.CenterX = 30 // the west wing would be at a negative x
	if _, err := Build(p); !errors.Is(err, ErrInvalidParams) || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("west edge: %v", err)
	}
	p = testParams(SizeLarge)
	p.CenterZ = 15360 - 50 // the tower would be north of the map
	if _, err := Build(p); err == nil || !strings.Contains(err.Error(), ClassTower) {
		t.Fatalf("north edge: %v", err)
	}
	p.Extras.CommentaryTower = false
	p.CenterZ = 15360 - 60 // without the tower the flags and stands still fit
	if _, err := Build(p); err != nil {
		t.Fatalf("north edge without the tower: %v", err)
	}
	p = testParams(SizeMedium)
	p.AltitudeY = MaxAltitudeY
	p.Offsets.TowerY = 1
	if _, err := Build(p); err == nil || !strings.Contains(err.Error(), "altitude") {
		t.Fatalf("altitude: %v", err)
	}
	p = testParams(SizeMedium)
	p.AltitudeY = 2000
	if err := p.Validate(); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("altitude validation: %v", err)
	}
	p = testParams(SizeMedium)
	p.MapKey = "sakhal"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "sakhal") {
		t.Fatalf("map validation: %v", err)
	}
}

func TestAltitudeRangeMatchesShop(t *testing.T) {
	if MinAltitudeY != nitradodelivery.MinAltitude || MaxAltitudeY != nitradodelivery.MaxAltitude {
		t.Fatalf("stadium altitude range %v..%v differs from the shop's %v..%v", MinAltitudeY, MaxAltitudeY, nitradodelivery.MinAltitude, nitradodelivery.MaxAltitude)
	}
}

func TestCustomItemValidationAndWarnings(t *testing.T) {
	p := testParams(SizeMedium)
	p.LockerKit = LockerCustom
	for name, items := range map[string][]Item{
		"empty":            {},
		"bad name":         {{"custom/model.p3d", 1}},
		"zero count":       {{"M4A1", 0}},
		"too many units":   {{"M4A1", 30}, {"Morphine", 11}},
		"too many entries": make([]Item, MaxCustomEntries+1),
	} {
		if name == "too many entries" {
			for i := range items {
				items[i] = Item{"BandageDressing", 1}
			}
		}
		p.CustomLockerItems = items
		if err := p.Validate(); !errors.Is(err, ErrInvalidParams) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// 40 items pack closer than 1.1 m and the preview says so; an unknown name is unverified.
	p.CustomLockerItems = []Item{{"M4A1", 20}, {"Mag_STANAG_30Rnd", 18}, {"Made_Up_Item", 2}}
	l := mustBuild(t, p)
	joined := strings.Join(l.Preview.Warnings, "\n")
	if !strings.Contains(joined, "Made_Up_Item") || !strings.Contains(joined, "packed") {
		t.Fatalf("warnings: %v", l.Preview.Warnings)
	}
	// Every item lies on a tunnel floor: inside the tunnel's u span and within its width.
	L := 80.0
	for _, o := range l.Objects {
		if StructureClasses[o.Name] {
			continue
		}
		u, v := local(p, o)
		au := math.Abs(u)
		if au < L/2+tunnelStart+0.5 || au > L/2+tunnelStart+2*containerLength+tunnelGap-0.5 {
			t.Fatalf("%s at u=%.2f is outside the tunnel", o.Name, u)
		}
		if av := math.Abs(math.Abs(v) - 3.4); av > containerWidth/2-0.3 {
			t.Fatalf("%s at v=%.2f is outside the tunnel width", o.Name, v)
		}
		if o.Pos[1] != 339.25 {
			t.Fatalf("%s at altitude %v, want altitude + itemY", o.Name, o.Pos[1])
		}
	}
	// The custom clothing kit goes to both wings; the item list reports it per room.
	p.ClothingKit, p.CustomClothingItems = ClothingCustom, []Item{{"TTSKOJacket_Camo", 2}}
	l = mustBuild(t, p)
	found := false
	for _, it := range l.Preview.Items {
		if it.ClassName == "TTSKOJacket_Camo" && it.Room == RoomClothingRoom && it.Count == 4 {
			found = true
		}
	}
	if !found {
		t.Fatalf("items: %+v", l.Preview.Items)
	}
}

func TestKitsAndZones(t *testing.T) {
	p := testParams(SizeMedium)
	p.LockerKit, p.ClothingKit = LockerM4PlusSidearm, ClothingTactical
	l := mustBuild(t, p)
	counts := map[string]int{}
	for _, it := range l.Preview.Items {
		counts[it.Room+"/"+it.ClassName] = it.Count
	}
	for key, n := range map[string]int{"LOCKER_ROOM/M4A1": 4, "LOCKER_ROOM/FNX45": 4, "LOCKER_ROOM/Mag_FNX45_15Rnd": 4, "LOCKER_ROOM/Mag_STANAG_30Rnd": 8, "CLOTHING_ROOM/TTSKOJacket_Camo": 4, "CLOTHING_ROOM/BallisticHelmet_Green": 4} {
		if counts[key] != n {
			t.Fatalf("%s: %d, want %d (%v)", key, counts[key], n, counts)
		}
	}
	kinds := map[string]bool{}
	for _, z := range l.Preview.Zones {
		kinds[z.Kind] = true
		if len(z.Polygon) != 4 || z.Name == "" {
			t.Fatalf("zone %+v", z)
		}
	}
	for _, k := range []string{ZoneArena, ZoneGateRed, ZoneGateBlue, ZoneWingRed, ZoneWingBlue, ZoneLockerRoomRed, ZoneClothingRoomRed, ZoneLockerRoomBlue, ZoneClothingRoomBlue, ZoneCommentary, ZoneStandNorthWest, ZoneStandNorthEast, ZoneStandSouthWest, ZoneStandSouthEast} {
		if !kinds[k] {
			t.Fatalf("missing zone %s in %v", k, kinds)
		}
	}
	if len(l.Preview.Footprint) != 4 {
		t.Fatalf("footprint %v", l.Preview.Footprint)
	}
	// The red (west) clothing room holds the red tracksuit, the blue one the blue tracksuit.
	p = testParams(SizeMedium)
	l = mustBuild(t, p)
	for _, o := range l.Objects {
		u, _ := local(p, o)
		if strings.HasPrefix(o.Name, "TrackSuit") && ((u < 0) != strings.HasSuffix(o.Name, "_Red")) {
			t.Fatalf("%s at u=%.2f is in the wrong wing", o.Name, u)
		}
	}
	// Extras off: no stairs, tower, flags or pillar, and no zones for them.
	p.Extras = Extras{}
	l = mustBuild(t, p)
	for _, o := range l.Objects {
		switch o.Name {
		case ClassStairs, ClassTower, ClassFlag, ClassMap:
			t.Fatalf("%s built with extras off", o.Name)
		}
	}
	for _, z := range l.Preview.Zones {
		if z.Kind == ZoneCommentary || strings.HasPrefix(z.Kind, "STAND_") {
			t.Fatalf("zone %s with extras off", z.Kind)
		}
	}
	if l.Preview.ObjectCount != 115-13 {
		t.Fatalf("%d objects with extras off", l.Preview.ObjectCount)
	}
}

func TestParseFillsDefaultsAndRefusesUnknownFields(t *testing.T) {
	p, err := Parse([]byte(`{"centerX":4618,"centerZ":10439,"altitudeY":339.2,"yawDeg":-90,"size":"small","extras":{"flags":false},"offsets":{"wallY":-0.5}}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Size != SizeSmall || p.Cover != CoverLight || p.LockerKit != LockerM4Only || p.ClothingKit != ClothingRedBlueCorners || p.AltitudeSource != "MANUAL" {
		t.Fatalf("defaults: %+v", p)
	}
	if p.YawDeg != 270 || p.Extras.Flags || !p.Extras.Stairs || !p.Extras.CommentaryTower || !p.Extras.MapPillar {
		t.Fatalf("yaw/extras: %+v", p)
	}
	if p.Offsets.WallY != -0.5 || p.Offsets.ItemY != DefaultItemY {
		t.Fatalf("offsets: %+v", p.Offsets)
	}
	if p.MapKey != "" || p.Map() != DefaultMapKey {
		t.Fatalf("map: %q", p.MapKey)
	}
	for name, body := range map[string]string{
		"unknown field":   `{"centerX":1,"centerZ":1,"altitudeY":1,"y":3}`,
		"no centre":       `{"altitudeY":1}`,
		"no altitude":     `{"centerX":1,"centerZ":1}`,
		"not json":        `{`,
		"bad size":        `{"centerX":1,"centerZ":1,"altitudeY":1,"size":"HUGE"}`,
		"bad source":      `{"centerX":1,"centerZ":1,"altitudeY":1,"altitudeSource":"GUESS"}`,
		"custom no items": `{"centerX":100,"centerZ":100,"altitudeY":1,"lockerKit":"CUSTOM"}`,
		"offset too big":  `{"centerX":100,"centerZ":100,"altitudeY":1,"offsets":{"itemY":9}}`,
	} {
		if _, err := Parse([]byte(body)); !errors.Is(err, ErrInvalidParams) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A stored Params round-trips through JSON unchanged.
	raw, _ := json.Marshal(p)
	again, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(again) != fmt.Sprint(p) {
		t.Fatalf("round trip:\n%+v\n%+v", p, again)
	}
}

func TestRenderAndParseFile(t *testing.T) {
	l := mustBuild(t, testParams(SizeSmall))
	raw := Render(l.Objects)
	if !bytes.HasSuffix(raw, []byte("\n")) || !bytes.HasPrefix(raw, []byte("{\n  \"Objects\": [\n    {\n      \"name\": ")) {
		t.Fatalf("render shape:\n%s", raw[:80])
	}
	if string(EmptyFile()) != "{\n  \"Objects\": []\n}\n" {
		t.Fatalf("empty file: %q", EmptyFile())
	}
	f, err := ParseFile(raw)
	if err != nil || len(f.Objects) != len(l.Objects) || !bytes.Equal(Render(f.Objects), raw) {
		t.Fatalf("parse round trip: %v", err)
	}
	if f, err := ParseFile(nil); err != nil || len(f.Objects) != 0 {
		t.Fatalf("empty: %v", err)
	}
	if _, err := ParseFile([]byte(`{"Objects":[{"name":"X","pos":[1,2,3],"ypr":[0,0,0],"scale":1,"enableCEPersistency":false,"customString":"somebody:else"}]}`)); !errors.Is(err, ErrForeignFile) {
		t.Fatalf("foreign: %v", err)
	}
	if _, err := ParseFile([]byte(`{"Objects":[{"name":"X","extra":1}]}`)); !errors.Is(err, ErrInvalidFile) {
		t.Fatalf("invalid: %v", err)
	}
	// Positions carry at most three decimals and a yaw stays in [0, 360).
	for _, o := range l.Objects {
		for _, c := range o.Pos {
			if math.Abs(c*1000-math.Round(c*1000)) > 1e-6 {
				t.Fatalf("coordinate %v has more than three decimals", c)
			}
		}
		if o.Ypr[0] < 0 || o.Ypr[0] >= 360 {
			t.Fatalf("yaw %v", o.Ypr[0])
		}
	}
	// The preview's class list is sorted and sums to the structure count.
	names := make([]string, 0, len(l.Preview.Classes))
	total := 0
	for _, c := range l.Preview.Classes {
		names = append(names, c.ClassName)
		total += c.Count
	}
	if !sort.StringsAreSorted(names) || total != len(l.Objects)-60 {
		t.Fatalf("classes %v (total %d)", names, total)
	}
}
