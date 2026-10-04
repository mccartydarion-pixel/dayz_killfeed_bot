package progression

import (
	"math"
	"sort"
	"strings"
)

// Zone is one territory: a named place on the map and the circle around it that can be held.
type Zone struct {
	Key     string  `json:"key"`
	Name    string  `json:"name"`
	X       float64 `json:"x"`
	Z       float64 `json:"z"`
	RadiusM float64 `json:"radius"`
}

func zone(name string, x, z, r float64) Zone {
	key := strings.ToLower(strings.NewReplacer(" ", "-", "'", "", "ó", "o").Replace(name))
	return Zone{Key: key, Name: name, X: x, Z: z, RadiusM: r}
}

// Zones are the places worth fighting over: the cities, the airfields and military bases and a few
// large towns. Centres come from the website's place labels (lib/maps/places); a kill counts for a
// zone when it happened inside its circle, and for the nearest centre where circles overlap.
var mapZones = map[string][]Zone{
	"chernarusplus": {
		zone("Chernogorsk", 6650, 2600, 700),
		zone("Elektrozavodsk", 10350, 2250, 650),
		zone("Berezino", 12200, 9100, 650),
		zone("Novodmitrovsk", 11650, 14400, 650),
		zone("Severograd", 8000, 14400, 550),
		zone("Zelenogorsk", 2750, 5250, 600),
		zone("Svetlojarsk", 13900, 13200, 550),
		zone("NWAF", 4600, 10400, 900),
		zone("NEAF", 12050, 12600, 700),
		zone("Tisy", 1600, 14000, 600),
		zone("Balota airstrip", 4900, 2450, 450),
		zone("Kamensk military", 7700, 15000, 350),
		zone("Pavlovo military", 1800, 3450, 350),
		zone("Veresnik", 4500, 8150, 400),
		zone("Myshkino tents", 1850, 7900, 350),
		zone("Stary Sobor", 6100, 7700, 450),
		zone("Vybor", 3800, 8900, 400),
		zone("Gorka", 9500, 8800, 400),
		zone("Krasnostav", 11200, 12300, 400),
		zone("Green Mountain", 3700, 5950, 350),
	},
	"enoch": {
		zone("Nadbór", 6050, 1900, 600),
		zone("Topolin", 2000, 1300, 550),
		zone("Brena", 10900, 2900, 600),
		zone("Sitnik", 4600, 5000, 550),
		zone("Lukow", 10400, 8700, 550),
		zone("Swarog airfield", 11800, 11900, 800),
		zone("Radunin military", 1700, 9500, 600),
		zone("Gliniska", 2300, 8200, 400),
		zone("Polana", 8400, 6500, 400),
		zone("Kolembrody", 6500, 10900, 400),
		zone("Tarnow", 7000, 9000, 400),
		zone("Bielawa", 3900, 10700, 400),
	},
}

// Zones returns the territory zones of a map; an unknown or unset map uses Chernarus, the same
// fallback the live map draws.
func Zones(mapKey string) []Zone {
	if z, ok := mapZones[strings.ToLower(strings.TrimSpace(mapKey))]; ok {
		return z
	}
	return mapZones["chernarusplus"]
}

// ZoneAt returns the zone a position falls in (the nearest centre among the circles that contain
// it) and false when it is in none.
func ZoneAt(zones []Zone, x, z float64) (Zone, bool) {
	best, found, bestD := Zone{}, false, math.MaxFloat64
	for _, zn := range zones {
		d := math.Hypot(x-zn.X, z-zn.Z)
		if d <= zn.RadiusM && d < bestD {
			best, found, bestD = zn, true, d
		}
	}
	return best, found
}

// FactionScore is one faction's points in a zone over the scoring window.
type FactionScore struct {
	FactionID int64
	Points    int
}

// DecideHolder applies the capture rule to one zone. holder is the faction holding it (0 = nobody)
// and scores are every faction's points in the window. A challenger takes the zone with at least
// minPoints and strictly more points than the holder; a tie never changes hands. A holder that
// scored nothing in the whole window loses the zone (it becomes neutral) unless someone takes it.
func DecideHolder(holder int64, scores []FactionScore, minPoints int) int64 {
	held := 0
	for _, s := range scores {
		if s.FactionID == holder {
			held = s.Points
		}
	}
	sorted := append([]FactionScore(nil), scores...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Points != sorted[j].Points {
			return sorted[i].Points > sorted[j].Points
		}
		return sorted[i].FactionID < sorted[j].FactionID
	})
	for i, s := range sorted {
		if s.FactionID == holder {
			continue
		}
		// A tie for first place between challengers takes nothing.
		if i+1 < len(sorted) && sorted[i+1].Points == s.Points && sorted[i+1].FactionID != holder {
			break
		}
		if s.Points >= minPoints && s.Points > held {
			return s.FactionID
		}
		break
	}
	if holder != 0 && held == 0 {
		return 0
	}
	return holder
}

// IncomeShares splits a zone's daily income between the faction's members: everyone gets the same
// whole number of points, at least 1 when there is any income.
func IncomeShares(income int64, members int) int64 {
	if income <= 0 || members <= 0 {
		return 0
	}
	if share := income / int64(members); share > 0 {
		return share
	}
	return 1
}
