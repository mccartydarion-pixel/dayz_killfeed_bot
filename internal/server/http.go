package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
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
	// /api/v1/status exposes runtime internals (bot name, log paths, DB pool
	// stats, Nitrado status), so it needs the same WEBSITE_API_SECRET bearer
	// as /api/runtime/status. /health, /live and /ready stay open: Railway's
	// healthcheck and readiness probes carry no credentials.
	mux.HandleFunc("/api/v1/status", requireWebsiteBearer(cfg, state.StatusHandler))

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

// Handler returns the server's mux, so tests can drive the registered routes without a listener.
func (s *Server) Handler() http.Handler {
	if s == nil || s.mux == nil {
		return http.NotFoundHandler()
	}
	return s.mux
}

// requireWebsiteBearer wraps next so it runs only for a request carrying
// "Authorization: Bearer <WEBSITE_API_SECRET>" (constant-time compare, the
// same rule as app.validBearerToken). An unset secret refuses everything.
func requireWebsiteBearer(cfg *config.Config, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg == nil || !validBearer(r.Header.Get("Authorization"), cfg.WebsiteAPISecret) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

// validBearer reports whether header is exactly "Bearer <secret>", compared
// in constant time. An empty secret never matches.
func validBearer(header, secret string) bool {
	const prefix = "Bearer "
	if secret == "" || !strings.HasPrefix(header, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, prefix)), []byte(secret)) == 1
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
		// ctx is already done here, so derive the drain deadline from a fresh
		// context: in-flight requests get shutdownTimeout to finish, then the
		// listener is closed regardless.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = s.Shutdown(shutdownCtx)
	}()
	return s.httpServer.ListenAndServe()
}

// shutdownTimeout bounds how long a graceful Shutdown waits for in-flight
// requests before the server is closed anyway.
const shutdownTimeout = 10 * time.Second

// Shutdown gracefully stops the HTTP server. It waits at most shutdownTimeout
// for in-flight requests even when ctx has no deadline of its own.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.httpServer == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()
	return s.httpServer.Shutdown(ctx)
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}
