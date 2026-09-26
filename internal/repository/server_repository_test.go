package repository

import (
	"errors"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/linking"
)

func TestResolveLinkServer(t *testing.T) {
	cases := []struct {
		name     string
		selected int64
		active   []int64
		want     int64
		wantErr  error
	}{
		{"no active server", 0, nil, 0, linking.ErrNoConnectedServer},
		{"stale selection, no active server", 9, nil, 0, linking.ErrNoConnectedServer},
		{"single active server", 0, []int64{5}, 5, nil},
		{"selection wins among several", 6, []int64{5, 6}, 6, nil},
		{"inactive selection falls back to the only active", 9, []int64{5}, 5, nil},
		{"several and no selection is ambiguous", 0, []int64{5, 6}, 0, linking.ErrMultipleConnectedServers},
	}
	for _, tc := range cases {
		got, err := resolveLinkServer(1, tc.selected, tc.active)
		if got != tc.want || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}
