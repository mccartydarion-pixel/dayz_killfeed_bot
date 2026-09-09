package analytics

import "testing"

func TestRecentForm(t *testing.T) {
	if Form(20, 5) != "HOT" {
		t.Fatal("hot")
	}
	if Form(5, 10) != "COLD" {
		t.Fatal("cold")
	}
	if Form(3, 3) != "STEADY" {
		t.Fatal("small sample should be steady")
	}
}
func TestHeadshotRate(t *testing.T) {
	if Rate(2, 10) != 20 {
		t.Fatal("rate")
	}
	if Rate(0, 0) != 0 {
		t.Fatal("zero")
	}
}
