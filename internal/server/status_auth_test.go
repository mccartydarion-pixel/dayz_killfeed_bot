package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/config"
)

func TestStatusRouteRequiresWebsiteBearer(t *testing.T) {
	const secret = "status-route-test-secret-0123456789abcdef"
	cases := []struct {
		name   string
		secret string
		auth   string
		want   int
	}{
		{"no header", secret, "", http.StatusUnauthorized},
		{"wrong secret", secret, "Bearer nope", http.StatusUnauthorized},
		{"not bearer", secret, secret, http.StatusUnauthorized},
		{"secret unset refuses", "", "Bearer ", http.StatusUnauthorized},
		{"valid bearer", secret, "Bearer " + secret, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := New(&config.Config{Port: "0", WebsiteAPISecret: tc.secret}, NewState())
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rr.Code, tc.want, rr.Body.String())
			}
			if tc.want == http.StatusUnauthorized && len(rr.Body.Bytes()) > 0 {
				if body := rr.Body.String(); strings.Contains(body, "bot_username") || strings.Contains(body, "log_path") {
					t.Fatalf("unauthorized response leaked status fields: %s", body)
				}
			}
		})
	}
}

func TestHealthAndProbesStayUnauthenticated(t *testing.T) {
	srv, err := New(&config.Config{Port: "0", WebsiteAPISecret: "status-route-test-secret-0123456789abcdef"}, NewState())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/health", "/live", "/ready"} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code == http.StatusUnauthorized {
			t.Fatalf("%s must not require auth, got 401", path)
		}
	}
}
