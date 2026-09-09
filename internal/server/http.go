package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/yourname/dayz-killfeed/internal/config"
)

// Server wraps the HTTP server.
type Server struct {
	httpServer *http.Server
	config     *config.Config
	state      *State
}

// New creates the HTTP server with tuned timeouts.
func New(cfg *config.Config, state *State) (*Server, error) {
	if cfg == nil {
		return nil, configLoadError()
	}
	if state == nil {
		state = NewState()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", HealthHandler)
	mux.HandleFunc("/live", LiveHandler)
	mux.HandleFunc("/ready", state.ReadyHandler)
	mux.HandleFunc("/api/v1/status", state.StatusHandler)

	addr := "0.0.0.0:" + cfg.Port
	if cfg.Port == "" {
		addr = "0.0.0.0:8080"
	}

	s := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 3 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	return &Server{httpServer: s, config: cfg, state: state}, nil
}

func configLoadError() error {
	return nil
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if s == nil || s.httpServer == nil {
		return nil
	}
	go func() {
		<-ctx.Done()
		_ = s.Shutdown(context.Background())
	}()
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}
