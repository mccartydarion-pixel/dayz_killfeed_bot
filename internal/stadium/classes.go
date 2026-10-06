package stadium

import "regexp"

// Structure class names the layout may emit. Every one appears in community object-spawner files
// that run on console DayZ servers (scalespeeder's NEAF maze, Svetlojarsk weapons showcase and the
// oil-rig/sea-platform files, all written for "Console and PC"), so the game host creates them from
// a cfggameplay.json object spawner. Build emits nothing outside this list (TestBuildEmitsOnly
// AllowlistedClasses). The sizes in the comments are the measurements the layout is drawn with;
// they come from the standard 20 ft ISO container and the model names, not from an in-game check.
const (
	// ClassWall is a 20 m stone castle wall segment. Its length runs along the model's X axis, so
	// yaw 0 is an east-west wall and yaw 90 a north-south one (the oil-rig file lays rows of them
	// end to end at a 20 m pitch along bearing 90+yaw).
	ClassWall = "Land_Castle_Wall1_20"
	// ClassContainerOpen is the walk-through container (both ends open), 6.06 x 2.44 m; its long
	// axis runs along the model's forward axis, so yaw 0 stands north-south and yaw 90 east-west
	// (the maze file lays them end to end at a 6.1-6.5 m pitch along bearing yaw).
	ClassContainerOpen = "Land_Container_1Aoh"
	// ClassContainerGate is a closed container, 6.06 x 2.44 m, same axes as ClassContainerOpen.
	ClassContainerGate = "Land_Container_1Bo"
	// ClassContainerSide is a closed container of another colour, 6.06 x 2.44 m, same axes.
	ClassContainerSide = "Land_Container_1Mo"
	// ClassMap is a wall-mounted school map; four of them back to back make the centre pillar.
	ClassMap = "StaticObj_Furniture_school_map"
	// ClassCrate is a wooden crate road block, about 1 m square; three make a cover cluster.
	ClassCrate = "Land_Roadblock_WoodenCrate"
	// ClassBlocks is the long row of concrete road-block cubes, about 6 x 1 m.
	ClassBlocks = "staticobj_roadblock_cncblocks_long"
	// ClassWreck is a wrecked UAZ, about 4.5 x 2 m, used as heavy cover.
	ClassWreck = "Land_Wreck_Uaz"
	// ClassStairs is the stone castle staircase used as a stand, about 6 x 3 m (a guess).
	ClassStairs = "Land_Castle_Stairs"
	// ClassTower is the small military air-traffic-control tower, about 8 x 8 m (a guess).
	ClassTower = "Land_Mil_ATC_Small"
	// ClassFlag is the Chernarus flag on a pole.
	ClassFlag = "StaticObj_Furniture_flag_chernarus_pole"
)

// StructureClasses is the allowlist of structures. Build only ever emits these and the item class
// names of the chosen kits.
var StructureClasses = map[string]bool{
	ClassWall: true, ClassContainerOpen: true, ClassContainerGate: true, ClassContainerSide: true, ClassMap: true,
	ClassCrate: true, ClassBlocks: true, ClassWreck: true, ClassStairs: true, ClassTower: true, ClassFlag: true,
}

// VerifiedItems are the item class names of the built-in kits: vanilla DayZ types (types.xml)
// known to exist on console. BandageDressing is the one the Champion Shop canary spawned and a
// player picked up (docs/SHOP_DELIVERY_WORKER_DESIGN.md). A custom item outside this list is
// accepted when its name is well formed, and the preview warns that it is unverified.
var VerifiedItems = map[string]bool{
	"M4A1": true, "Mag_STANAG_30Rnd": true, "AmmoBox_556x45_20Rnd": true, "M4_T3NRDSOptic": true, "M4_RISHndgrd": true, "M4_MPBttstck": true,
	"BandageDressing": true, "Morphine": true, "FNX45": true, "Mag_FNX45_15Rnd": true,
	"TrackSuitJacket_Red": true, "TrackSuitPants_Red": true, "TrackSuitJacket_Blue": true, "TrackSuitPants_Blue": true,
	"BallisticHelmet_Black": true, "BallisticHelmet_Green": true, "PlateCarrierVest": true, "TacticalGloves_Black": true, "CombatBoots_Black": true,
	"TTSKOJacket_Camo": true, "TTSKOPants": true,
}

// Allowed reports whether a class name is a structure of the layout or a verified item.
func Allowed(className string) bool { return StructureClasses[className] || VerifiedItems[className] }

// classNameRe is the shape of a DayZ config class name (the same rule the Shop applies): letters,
// digits and underscores, 2 to 64 characters. A path separator would make the spawner create a
// static P3D model instead of an item, so anything else is refused.
var classNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{1,63}$`)

// ValidClassName reports whether name has the shape of a DayZ class name.
func ValidClassName(name string) bool { return classNameRe.MatchString(name) }
