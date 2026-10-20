package hotzone

import "testing"

func TestPickChoosesBusiestCellAtOrAboveThreshold(t *testing.T) {
	cells := []Cell{{CellX: 3, CellZ: 9, Count: 4}, {CellX: 15, CellZ: 25, Count: 9}, {CellX: 1, CellZ: 1, Count: 9}}
	got, ok := Pick(cells, 6)
	if !ok || got != (Cell{CellX: 1, CellZ: 1, Count: 9}) {
		t.Fatalf("Pick = %+v %v, want the lowest-coordinate cell among the tied busiest", got, ok)
	}
	if _, ok := Pick(cells, 10); ok {
		t.Fatal("a cell below the threshold opened a hot zone")
	}
	if got, ok := Pick(cells, 9); !ok || got.Count != 9 {
		t.Fatalf("exactly the threshold must qualify: %+v %v", got, ok)
	}
	if _, ok := Pick(nil, 1); ok {
		t.Fatal("no cells opened a hot zone")
	}
	if _, ok := Pick(cells, 0); ok {
		t.Fatal("a zero threshold opened a hot zone")
	}
	if cells[0].CellX != 3 {
		t.Fatal("Pick reordered its input")
	}
}

func TestCenterAndName(t *testing.T) {
	x, z := Center(Cell{CellX: 15, CellZ: 25})
	if x != 7750 || z != 12750 {
		t.Fatalf("Center = %v, %v", x, z)
	}
	if got := Name(x, z); got != "Hot Zone 7750 / 12750" {
		t.Fatalf("Name = %q", got)
	}
}
