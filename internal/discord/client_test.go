package discord

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// withFakeGuildEndpoint redirects discordgo's guild REST calls to a local
// test server for the duration of the test, restoring the real endpoint
// afterward. EndpointGuilds is an exported discordgo var maintainers
// explicitly document as safe to override ("you may modify them if
// needed" - endpoints.go) - the standard way to test discordgo REST calls
// without hitting the real Discord API.
func withFakeGuildEndpoint(t *testing.T, handler http.HandlerFunc) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	original := discordgo.EndpointGuilds
	discordgo.EndpointGuilds = srv.URL + "/guilds/"
	t.Cleanup(func() { discordgo.EndpointGuilds = original })
	return &calls
}

func TestStartNilReceiverAndSessionReturnError(t *testing.T) {
	var nilClient *Client
	if err := nilClient.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "discord session not initialized") {
		t.Fatalf("unexpected nil receiver error: %v", err)
	}
	if err := (&Client{}).Start(context.Background()); err == nil || !strings.Contains(err.Error(), "discord session not initialized") {
		t.Fatalf("unexpected nil session error: %v", err)
	}
}
func TestNewInitializesSession(t *testing.T) {
	client, err := New("test-token")
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || client.session == nil {
		t.Fatal("constructor must initialize Discord session")
	}
	if _, err := New(" "); err == nil {
		t.Fatal("empty token should be rejected")
	}
}

func TestNewRequestsOnlyConfiguredMembersIntent(t *testing.T) {
	without, err := New("test-token", false)
	if err != nil {
		t.Fatal(err)
	}
	if without.GuildMembersIntentRequested() {
		t.Fatal("members intent should not be requested when disabled")
	}
	with, err := New("test-token", true)
	if err != nil {
		t.Fatal(err)
	}
	if !with.GuildMembersIntentRequested() {
		t.Fatal("members intent should be requested when enabled")
	}
}

func TestWaitForSessionUserReturnsTrueImmediatelyWhenAlreadyReady(t *testing.T) {
	session := &discordgo.Session{State: discordgo.NewState()}
	session.State.User = &discordgo.User{ID: "1"}
	if !waitForSessionUser(context.Background(), session, time.Second) {
		t.Fatal("expected true when State.User is already populated")
	}
}

func TestWaitForSessionUserWaitsForReadyThenSucceeds(t *testing.T) {
	session := &discordgo.Session{State: discordgo.NewState()}
	go func() {
		time.Sleep(20 * time.Millisecond)
		session.State.User = &discordgo.User{ID: "1"}
	}()
	if !waitForSessionUser(context.Background(), session, time.Second) {
		t.Fatal("expected true once READY populates State.User")
	}
}

func TestWaitForSessionUserTimesOutWhenReadyNeverArrives(t *testing.T) {
	session := &discordgo.Session{State: discordgo.NewState()}
	if waitForSessionUser(context.Background(), session, 30*time.Millisecond) {
		t.Fatal("expected false when READY never arrives before the deadline")
	}
}

func TestHasGuildCachedReadsLocalStateOnly(t *testing.T) {
	var nilClient *Client
	if nilClient.HasGuildCached("123") {
		t.Fatal("expected a nil receiver to report false")
	}
	if (&Client{}).HasGuildCached("123") {
		t.Fatal("expected a nil session to report false")
	}
	sessionNoState := &discordgo.Session{}
	if (&Client{session: sessionNoState}).HasGuildCached("123") {
		t.Fatal("expected a nil session.State to report false")
	}

	session := &discordgo.Session{State: discordgo.NewState()}
	client := &Client{session: session}
	if client.HasGuildCached("") {
		t.Fatal("expected an empty guildID to report false")
	}
	if client.HasGuildCached("999") {
		t.Fatal("expected an uncached guild to report false")
	}
	if err := session.State.GuildAdd(&discordgo.Guild{ID: "123"}); err != nil {
		t.Fatal(err)
	}
	if !client.HasGuildCached("123") {
		t.Fatal("expected a cached guild to report true")
	}
	if client.HasGuildCached("456") {
		t.Fatal("expected a different, still-uncached guild to report false")
	}
}

// TestVerifyGuildCachedSkipsRESTEntirely is case A: a guild already in the
// local gateway state cache must report GuildFound=true without ever
// issuing a live REST call - the "already-installed bot" fast path.
func TestVerifyGuildCachedSkipsRESTEntirely(t *testing.T) {
	calls := withFakeGuildEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("expected zero REST calls for a cached guild")
	})

	client, err := New("test-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.session.State.GuildAdd(&discordgo.Guild{ID: "cached-guild"}); err != nil {
		t.Fatal(err)
	}

	result := client.Verify("cached-guild", "")
	if !result.GuildFound {
		t.Fatal("expected a cached guild to be found")
	}
	if calls.Load() != 0 {
		t.Fatalf("expected 0 REST calls, got %d", calls.Load())
	}
}

// TestVerifyGuildCacheMissFallsBackToRESTSuccess is case B: a guild absent
// from the cache (e.g. the local gateway state hasn't caught up yet) must
// still be found via a live REST fallback - the REST fallback this fix
// keeps, per section 1's explicit "do not remove" instruction.
func TestVerifyGuildCacheMissFallsBackToRESTSuccess(t *testing.T) {
	calls := withFakeGuildEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"rest-guild"}`))
	})

	client, err := New("test-token")
	if err != nil {
		t.Fatal(err)
	}

	result := client.Verify("rest-guild", "")
	if !result.GuildFound {
		t.Fatal("expected the REST fallback to find the guild")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly 1 REST call on a cache miss, got %d", calls.Load())
	}
}

// TestVerifyGuildCacheMissAndRESTFailureIsNotFound is case C: only when
// BOTH the cache misses AND the REST fallback fails should GuildFound be
// false (section 1E).
func TestVerifyGuildCacheMissAndRESTFailureIsNotFound(t *testing.T) {
	calls := withFakeGuildEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	client, err := New("test-token")
	if err != nil {
		t.Fatal(err)
	}

	result := client.Verify("missing-guild", "")
	if result.GuildFound {
		t.Fatal("expected GuildFound=false when both cache and REST miss")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly 1 REST call, got %d", calls.Load())
	}
}

// TestVerifyEmptyGuildIDSkipsVerificationEntirely preserves case A of the
// original behavior (section 1A): an empty guildID skips guild
// verification without attempting either the cache or REST.
func TestVerifyEmptyGuildIDSkipsVerificationEntirely(t *testing.T) {
	calls := withFakeGuildEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("expected zero REST calls when guildID is empty")
	})

	client, err := New("test-token")
	if err != nil {
		t.Fatal(err)
	}

	result := client.Verify("", "")
	if result.GuildFound {
		t.Fatal("expected GuildFound=false when guildID is empty")
	}
	if calls.Load() != 0 {
		t.Fatalf("expected 0 REST calls, got %d", calls.Load())
	}
}

func TestWaitForSessionUserStopsOnContextCancel(t *testing.T) {
	session := &discordgo.Session{State: discordgo.NewState()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitForSessionUser(ctx, session, time.Second) {
		t.Fatal("expected false when context is already cancelled")
	}
}
