// Package dayzmap is the catalog of DayZ maps whose coordinate bounds Champion has verified
// (docs/SHOP_DELIVERY.md "Map validation"). A map that is not listed here - or listed without
// verified bounds - is unsupported: callers must reject coordinate input for it rather than guess.
//
// DayZ positions are (x, y, z) with y the altitude; a ground position on the map is the (x, z)
// pair, both in metres from the south-west corner (0, 0) of a square terrain. Champion never
// invents a y value.
package dayzmap

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// Map is one supported DayZ terrain. Keys are the engine world names (the part after the dot in
// a mission folder such as "dayzOffline.chernarusplus"), lower-case.
type Map struct {
	Key         string
	DisplayName string
	// SizeMetres is the verified edge length of the square terrain: valid x and z are
	// 0..SizeMetres inclusive.
	SizeMetres float64
}

// Bounds is the inclusive coordinate rectangle of a map.
type Bounds struct{ MinX, MaxX, MinZ, MaxZ float64 }

func (m Map) Bounds() Bounds { return Bounds{MinX: 0, MaxX: m.SizeMetres, MinZ: 0, MaxZ: m.SizeMetres} }

// supported lists only maps with verified terrain sizes: Chernarus+ (15360 m), Livonia, whose
// world name is "enoch" (12800 m), and Nasdara, the Badlands map of update 1.30, whose mission
// is dayzOffline.nasdara (16384 m). Sakhal and community maps are deliberately absent until their
// bounds are verified - an installation on them cannot take coordinate deliveries.
var supported = map[string]Map{
	"chernarusplus": {Key: "chernarusplus", DisplayName: "Chernarus", SizeMetres: 15360},
	"enoch":         {Key: "enoch", DisplayName: "Livonia", SizeMetres: 12800},
	"nasdara":       {Key: "nasdara", DisplayName: "Nasdara", SizeMetres: 16384},
}

// Lookup resolves a map key (case-insensitive). ok is false for an unknown or unverified map.
func Lookup(key string) (Map, bool) {
	m, ok := supported[strings.ToLower(strings.TrimSpace(key))]
	return m, ok
}

// worldNameRe is what a DayZ world name looks like (letters, digits, underscore).
var worldNameRe = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// ServerWorld is the world a server runs, from what Nitrado reports: the part after the last dot
// of its mission ("dayzOffline.nasdara", "dayzps_missions/dayzOffline.enoch"), else the map its
// query answer names. Lower-case; "" when neither says. The world may be one Champion does not
// support (Lookup decides).
func ServerWorld(mission, queryMap string) string {
	m := strings.ToLower(strings.TrimSpace(mission))
	if i := strings.LastIndexAny(m, "/\\"); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.LastIndex(m, "."); i >= 0 {
		if w := m[i+1:]; worldNameRe.MatchString(w) {
			return w
		}
	}
	if q := strings.ToLower(strings.TrimSpace(queryMap)); worldNameRe.MatchString(q) {
		return q
	}
	return ""
}

// Supported returns every supported map, sorted by key.
func Supported() []Map {
	out := make([]Map, 0, len(supported))
	for _, m := range supported {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Contains reports whether (x, z) is a finite ground position inside the map.
func (m Map) Contains(x, z float64) bool {
	if math.IsNaN(x) || math.IsNaN(z) || math.IsInf(x, 0) || math.IsInf(z, 0) {
		return false
	}
	b := m.Bounds()
	return x >= b.MinX && x <= b.MaxX && z >= b.MinZ && z <= b.MaxZ
}
