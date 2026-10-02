package admin

import (
	"context"
	"testing"
)

func TestStatusWithBuildsOnlyTheNamedDiagnostics(t *testing.T) {
	s := NewService(nil, nil)
	calls := map[string]int{}
	diag := func(name string) func(context.Context) map[string]any {
		return func(context.Context) map[string]any { calls[name]++; return map[string]any{} }
	}
	s.SetLinkDiagnostics(diag("link"))
	s.SetPresenceDiagnostics(diag("presence"))
	s.SetPipelineDiagnostics(diag("pipeline"))

	if out := s.StatusWith(context.Background()); len(calls) != 0 || out["uptime"] == nil {
		t.Fatalf("plain status ran diagnostics: %v", calls)
	}
	out := s.StatusWith(context.Background(), "presence_diagnostics")
	if calls["presence"] != 1 || calls["link"] != 0 || calls["pipeline"] != 0 || out["presence_diagnostics"] == nil {
		t.Fatalf("calls = %v", calls)
	}
	s.Status(context.Background())
	if calls["link"] != 1 || calls["presence"] != 2 || calls["pipeline"] != 1 {
		t.Fatalf("full status calls = %v", calls)
	}
}
