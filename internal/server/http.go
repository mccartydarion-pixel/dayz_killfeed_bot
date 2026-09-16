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
	mux        *http.ServeMux
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

	return &Server{httpServer: s, mux: mux, config: cfg, state: state}, nil
}

// Handle registers an additional route on the server's existing mux, so
// callers never bind a second listener/port alongside this one. Must be
// called before ListenAndServe starts serving.
func (s *Server) Handle(pattern string, handler http.HandlerFunc) {
	if s == nil || s.mux == nil || handler == nil {
		return
	}
	s.mux.HandleFunc(pattern, handler)
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
