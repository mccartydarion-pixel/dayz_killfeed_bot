package app

import (
	"testing"

	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/server"
)

// Go's ServeMux panics when two patterns conflict, and it does so at registration - which in
// production is process start. This registers every route exactly as New does, on one mux, so a
// new route that collides with an existing one fails here instead of on deploy.
func TestEveryRouteRegistersOnOneMux(t *testing.T) {
	cfg := &config.Config{Port: "0", WebsiteAPISecret: "test-secret"}
	srv, err := server.New(cfg, server.NewState())
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Config: cfg, HTTPServer: srv, State: server.NewState()}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("route registration panicked: %v", r)
		}
	}()
	a.registerRuntimeStatusAPI()
	a.registerSaaSAPI()
	a.registerAdminAPI()
}
