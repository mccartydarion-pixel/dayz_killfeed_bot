// Package hotzone decides when and where a hot zone opens (docs/HOT_ZONES.md). A hot zone is a
// short competitive event scored on kills inside a circle on the map; it opens by itself where the
// PvP heatmap shows a fight already happening. This package is the pure decision: no I/O, no clock.
package hotzone

import (
	"fmt"
	"sort"
)

// Resolution is the heatmap grid (metres) hot zones are detected on - one of the heatmap's own
// supported resolutions, coarse enough that one firefight lands in one cell.
const Resolution = 500

// Cell is one heatmap grid cell and the kills counted in it.
type Cell struct {
	CellX, CellZ int64
	Count        int64
}

// Pick returns the busiest cell with at least minKills kills. Ties go to the lowest cell
// coordinates so the same input always opens the same zone.
func Pick(cells []Cell, minKills int) (Cell, bool) {
	if len(cells) == 0 || minKills < 1 {
		return Cell{}, false
	}
	sorted := append([]Cell(nil), cells...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.CellX != b.CellX {
			return a.CellX < b.CellX
		}
		return a.CellZ < b.CellZ
	})
	if sorted[0].Count < int64(minKills) {
		return Cell{}, false
	}
	return sorted[0], true
}

// Center is the map position (metres) of the middle of a cell.
func Center(c Cell) (x, z float64) {
	return (float64(c.CellX) + 0.5) * Resolution, (float64(c.CellZ) + 0.5) * Resolution
}

// Name labels a hot zone by its centre in map metres, the form players read off a map tool.
func Name(x, z float64) string {
	return fmt.Sprintf("Hot Zone %.0f / %.0f", x, z)
}
