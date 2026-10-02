package nitrado

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestParsePriorityListTrimsAndDedupes(t *testing.T) {
	got := ParsePriorityList("Alpha\r\n  Bravo \r\n\r\nalpha\nCharlie\rDelta\r\n")
	want := []string{"Alpha", "Bravo", "Charlie", "Delta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := ParsePriorityList(""); got == nil || len(got) != 0 {
		t.Fatalf("an empty value must be an empty, non-nil list, got %#v", got)
	}
}

func TestValidPriorityName(t *testing.T) {
	for _, ok := range []string{"Alpha", "Some Gamer Tag", "x_X-1"} {
		if !ValidPriorityName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", " lead", "trail ", "two\nlines", "cr\rhere", "tab\there", strings.Repeat("a", 65)} {
		if ValidPriorityName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestPriorityListReadsOnlyThePrioritySetting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/services/svc1/gameservers" {
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"gameserver":{"settings":{"general":{"priority":"Alpha\r\nBravo","admin-password":"secret"},"config":{"password":"secret"}}}}}`))
	}))
	defer srv.Close()
	got, err := NewClient(srv.URL, "token", srv.Client()).PriorityList(context.Background(), "svc1")
	if err != nil {
		t.Fatalf("PriorityList: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"Alpha", "Bravo"}) {
		t.Fatalf("got %q", got)
	}
}

func TestPriorityListIsUnsupportedWhenTheSettingIsAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"gameserver":{"settings":{"general":{"expertMode":"false"}}}}}`))
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "token", srv.Client()).PriorityList(context.Background(), "svc1"); !errors.Is(err, ErrPriorityUnsupported) {
		t.Fatalf("expected ErrPriorityUnsupported, got %v", err)
	}
}

func TestSetPriorityListWritesTheWholeListAsOneSetting(t *testing.T) {
	var method, path, contentType string
	var sent map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, contentType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &sent)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := NewClient(srv.URL, "token", srv.Client()).SetPriorityList(context.Background(), "svc1", []string{"Alpha", "Bravo"}); err != nil {
		t.Fatalf("SetPriorityList: %v", err)
	}
	if method != http.MethodPost || path != "/services/svc1/gameservers/settings" || contentType != "application/json" {
		t.Fatalf("unexpected request: %s %s (%s)", method, path, contentType)
	}
	want := map[string]string{"category": "general", "key": "priority", "value": "Alpha\r\nBravo"}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent %v, want %v", sent, want)
	}
}

func TestSetPriorityListResendsTheBodyAfterARateLimit(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		if len(bodies) == 1 {
			w.Header().Set("Retry-After", "0.01")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := NewClient(srv.URL, "token", srv.Client()).SetPriorityList(context.Background(), "svc1", []string{"Alpha"}); err != nil {
		t.Fatalf("SetPriorityList: %v", err)
	}
	if len(bodies) != 2 || bodies[0] == "" || bodies[0] != bodies[1] {
		t.Fatalf("the retry must carry the same body, got %q", bodies)
	}
}

func TestSetPriorityListRefusesBadInput(t *testing.T) {
	client := NewClient("http://unused.invalid", "token", nil)
	if err := client.SetPriorityList(context.Background(), "", []string{"Alpha"}); err == nil {
		t.Error("expected an error for an empty service ID")
	}
	if err := client.SetPriorityList(context.Background(), "svc1", []string{"two\nlines"}); err == nil {
		t.Error("expected an error for a name with a line break")
	}
	if err := client.SetPriorityList(context.Background(), "svc1", make([]string, MaxPriorityEntries+1)); err == nil {
		t.Error("expected an error for an oversized list")
	}
}

func TestSetPriorityListSurfacesARejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	if err := NewClient(srv.URL, "token", srv.Client()).SetPriorityList(context.Background(), "svc1", []string{"Alpha"}); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}
