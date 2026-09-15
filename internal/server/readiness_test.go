package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadinessSatisfiedRequiresAllThree(t *testing.T) {
	cases := []struct {
		db, discord, handlers bool
		want                  bool
	}{
		{true, true, true, true},
		{false, true, true, false},
		{true, false, true, false},
		{true, true, false, false},
		{false, false, false, false},
	}
	for _, c := range cases {
		if got := readinessSatisfied(c.db, c.discord, c.handlers); got != c.want {
			t.Fatalf("readinessSatisfied(%v,%v,%v) = %v, want %v", c.db, c.discord, c.handlers, got, c.want)
		}
	}
}

func TestReadyHandlerNotReadyUntilHandlersReady(t *testing.T) {
	state := NewState()
	state.SetDatabase(true, 5, 5)
	state.SetDiscord(true, "bot", true, true, nil)

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rr := httptest.NewRecorder()
	state.ReadyHandler(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected not_ready before handlers install, got status %d", rr.Code)
	}

	state.SetHandlersReady(true)
	rr = httptest.NewRecorder()
	state.ReadyHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected ready once handlers install, got status %d", rr.Code)
	}
}
