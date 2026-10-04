// Package progression holds the pure rules behind daily and weekly challenges, the battle pass and
// territory control (docs/PROGRESSION.md): which challenges a day gets, which weapons count as a
// class, the territory zones of each map and who holds one, and battle pass levels. No I/O, no
// clock: callers pass the time in.
package progression

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"regexp"
	"strings"
	"time"
)

// Challenge periods.
const (
	PeriodDay  = "DAY"
	PeriodWeek = "WEEK"
)

// Challenge kinds. Each is measured from data the bot already records.
const (
	KindKills     = "KILLS"      // PvP kills
	KindHeadshots = "HEADSHOTS"  // PvP kills that were headshots
	KindLongRange = "LONG_RANGE" // PvP kills from at least Param metres
	KindWeapon    = "WEAPON"     // PvP kills with a weapon of class Class
	KindPlaytime  = "PLAYTIME"   // minutes played
	KindSurvive   = "SURVIVE"    // minutes alive in one life
	KindHotZone   = "HOT_ZONE"   // PvP kills scored inside a hot zone
	KindTerritory = "TERRITORY"  // PvP kills inside a territory zone
)

// Weapon classes a WEAPON challenge can ask for.
const (
	ClassSniper  = "SNIPER"
	ClassRifle   = "RIFLE"
	ClassSMG     = "SMG"
	ClassShotgun = "SHOTGUN"
	ClassPistol  = "PISTOL"
)

// weaponPatterns match the lower-cased weapon name (display name, else the raw class name). They
// are PostgreSQL-compatible regular expressions, so the same pattern runs in SQL and in Go.
var weaponPatterns = map[string]string{
	ClassSniper:  `mosin|tundra|m70|svd|vsd|vss|scout|cr-?527|cr-?550|sv-?98|sniper|winchester|repeater|blaze|ssg ?82|kar ?98|pioneer`,
	ClassRifle:   `m4-?a1|m4a1|\mm4\M|m16|akm|ak-?74|ak-?101|aks-?74|ak74|ak101|ka-?m|ka-?74|ka-?101|\maug\M|famas|\mfal\M|\mlar\M|m14|sg5|vikhr|sa-?58|\mm1\M|garand|\msks\M|\mdmr\M|assault rifle`,
	ClassSMG:     `mp-?5|mp5|\mump\M|pp-?19|bizon|cr-?61|skorpion|vz-?61|mp-?7|\msmg\M|sg5-?k|ump-?45`,
	ClassShotgun: `shotgun|bk-?43|bk-?133|bk-?12|mp-?133|vaiga|saiga|izh|sawed|sawn|\mbk\M`,
	ClassPistol:  `glock|cz ?75|cz-?75|deagle|desert eagle|pistol|fx-?45|mk ?ii|\mmkii\M|1911|\mp1\M|magnum|revolver|longhorn|ij-?70|makarov|derringer|flare gun|\mfnx\M`,
}

// WeaponPattern returns the PostgreSQL regular expression of a weapon class, "" for an unknown one.
func WeaponPattern(class string) string { return weaponPatterns[class] }

// goPattern is WeaponPattern in Go's syntax (\m and \M are PostgreSQL word boundaries).
func goPattern(class string) *regexp.Regexp {
	p := weaponPatterns[class]
	if p == "" {
		return nil
	}
	p = strings.NewReplacer(`\m`, `\b`, `\M`, `\b`).Replace(p)
	return regexp.MustCompile(p)
}

// classOrder is the order classes are tried in: names that could match twice ("SG5-K") go to the
// first class that matches.
var classOrder = []string{ClassShotgun, ClassSMG, ClassSniper, ClassPistol, ClassRifle}

// WeaponSQLPatterns returns, for SQL, the pattern a weapon of the class matches and the combined
// pattern of the classes tried before it, which it must not match ("" for the first class). This
// is WeaponClass's precedence, so SQL and Go classify every weapon the same way.
func WeaponSQLPatterns(class string) (match, exclude string) {
	var before []string
	for _, c := range classOrder {
		if c == class {
			return weaponPatterns[c], strings.Join(before, "|")
		}
		before = append(before, "("+weaponPatterns[c]+")")
	}
	return "", ""
}

// WeaponClass returns the class of a weapon name ("" when it matches none).
func WeaponClass(weapon string) string {
	lower := strings.ToLower(weapon)
	for _, class := range classOrder {
		if re := goPattern(class); re != nil && re.MatchString(lower) {
			return class
		}
	}
	return ""
}

var classNames = map[string]string{
	ClassSniper: "a sniper rifle", ClassRifle: "an assault rifle", ClassSMG: "an SMG", ClassShotgun: "a shotgun", ClassPistol: "a pistol",
}

// Challenge is one challenge of a day or a week.
type Challenge struct {
	// Key names the template, so the same challenge is recognised across periods.
	Key    string `json:"key"`
	Kind   string `json:"kind"`
	Target int    `json:"target"`
	// Param is the distance in metres of a LONG_RANGE challenge (0 otherwise).
	Param int `json:"param,omitempty"`
	// Class is the weapon class of a WEAPON challenge.
	Class string `json:"class,omitempty"`
	Title string `json:"title"`
}

// template is a challenge before its period sets the target.
type template struct {
	key, kind, class string
	param            int
	day, week        int
	needs            string // "" | "hotzone" | "territory"
	title            func(target, param int, class string) string
}

func killsTitle(what string) func(int, int, string) string {
	return func(n, _ int, _ string) string {
		if n == 1 {
			return "Get 1 " + strings.TrimSuffix(what, "s")
		}
		return fmt.Sprintf("Get %d %s", n, what)
	}
}

var templates = []template{
	{key: "kills", kind: KindKills, day: 3, week: 15, title: killsTitle("kills")},
	{key: "headshots", kind: KindHeadshots, day: 1, week: 6, title: killsTitle("headshot kills")},
	{key: "long-range", kind: KindLongRange, param: 200, day: 1, week: 5, title: func(n, d int, _ string) string {
		if n == 1 {
			return fmt.Sprintf("Kill a player from %d m or more", d)
		}
		return fmt.Sprintf("Kill %d players from %d m or more", n, d)
	}},
	{key: "sniper", kind: KindWeapon, class: ClassSniper, day: 2, week: 8, title: weaponTitle},
	{key: "rifle", kind: KindWeapon, class: ClassRifle, day: 3, week: 12, title: weaponTitle},
	{key: "smg", kind: KindWeapon, class: ClassSMG, day: 2, week: 8, title: weaponTitle},
	{key: "shotgun", kind: KindWeapon, class: ClassShotgun, day: 2, week: 8, title: weaponTitle},
	{key: "pistol", kind: KindWeapon, class: ClassPistol, day: 1, week: 5, title: weaponTitle},
	{key: "playtime", kind: KindPlaytime, day: 60, week: 600, title: func(n, _ int, _ string) string { return "Play for " + minutesText(n) }},
	{key: "survive", kind: KindSurvive, day: 120, week: 360, title: func(n, _ int, _ string) string { return "Survive " + minutesText(n) + " in one life" }},
	{key: "hot-zone", kind: KindHotZone, day: 1, week: 5, needs: "hotzone", title: func(n, _ int, _ string) string {
		if n == 1 {
			return "Get a kill inside a hot zone"
		}
		return fmt.Sprintf("Get %d kills inside hot zones", n)
	}},
	{key: "territory", kind: KindTerritory, day: 2, week: 10, needs: "territory", title: func(n, _ int, _ string) string {
		if n == 1 {
			return "Get a kill inside a territory"
		}
		return fmt.Sprintf("Get %d kills inside territories", n)
	}},
}

func weaponTitle(n, _ int, class string) string {
	if n == 1 {
		return "Get a kill with " + classNames[class]
	}
	return fmt.Sprintf("Get %d kills with %s", n, classNames[class])
}

func minutesText(m int) string {
	if m%60 == 0 {
		if m == 60 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", m/60)
	}
	if m > 60 {
		return fmt.Sprintf("%dh %dm", m/60, m%60)
	}
	return fmt.Sprintf("%d minutes", m)
}

// Features says which optional systems a server has on, so challenges that need them can be drawn.
type Features struct {
	HotZones  bool `json:"hotZones"`
	Territory bool `json:"territory"`
}

// PeriodStart is the start of the DAY (00:00 UTC) or WEEK (Monday 00:00 UTC) containing t.
func PeriodStart(period string, t time.Time) time.Time {
	day := t.UTC().Truncate(24 * time.Hour)
	if period == PeriodWeek {
		return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	}
	return day
}

// PeriodEnd is when the period that started at start ends.
func PeriodEnd(period string, start time.Time) time.Time {
	if period == PeriodWeek {
		return start.AddDate(0, 0, 7)
	}
	return start.AddDate(0, 0, 1)
}

// Generate draws count distinct challenges for a server's period. The same server, period and
// start always draw the same challenges; a different day draws a different set.
func Generate(serverID int64, period string, start time.Time, count int, f Features) []Challenge {
	pool := make([]template, 0, len(templates))
	for _, t := range templates {
		if (t.needs == "hotzone" && !f.HotZones) || (t.needs == "territory" && !f.Territory) {
			continue
		}
		pool = append(pool, t)
	}
	if count > len(pool) {
		count = len(pool)
	}
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%s|%s", serverID, period, start.UTC().Format("2006-01-02"))
	rng := rand.New(rand.NewSource(int64(h.Sum64() >> 1))) //nolint:gosec // a stable shuffle, not a secret
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	// At most one weapon challenge per period, so a day is not all guns.
	out := make([]Challenge, 0, count)
	weapon := false
	for _, t := range pool {
		if len(out) == count {
			break
		}
		if t.kind == KindWeapon {
			if weapon {
				continue
			}
			weapon = true
		}
		target := t.day
		if period == PeriodWeek {
			target = t.week
		}
		param := t.param
		if t.kind == KindLongRange && period == PeriodWeek {
			param = 300
		}
		out = append(out, Challenge{Key: t.key, Kind: t.kind, Target: target, Param: param, Class: t.class, Title: t.title(target, param, t.class)})
	}
	return out
}
