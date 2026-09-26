package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/security"
	"github.com/yourname/dayz-killfeed/internal/servers"
)

func TestConsumeFirstConnectIsOneShot(t *testing.T) {
	a := &App{}
	if a.consumeFirstConnect(5) {
		t.Fatal("expected no flag before markFirstConnect")
	}
	a.markFirstConnect(5)
	if !a.consumeFirstConnect(5) {
		t.Fatal("expected flag set after markFirstConnect")
	}
	if a.consumeFirstConnect(5) {
		t.Fatal("expected flag to be cleared after first consume")
	}
}

func TestConnectServerRequiresWorkerManager(t *testing.T) {
	a := &App{}
	if err := a.ConnectServer(context.Background(), 1); err == nil {
		t.Fatal("expected error when WorkerManager is not initialized")
	}
}

func TestConnectServerMarksFirstConnectAndStartsWorker(t *testing.T) {
	started := make(chan int64, 1)
	a := &App{}
	a.WorkerManager = servers.NewWorkerManager(func(ctx context.Context, id int64) error {
		started <- id
		<-ctx.Done()
		return nil
	})

	if err := a.ConnectServer(context.Background(), 42); err != nil {
		t.Fatalf("ConnectServer failed: %v", err)
	}
	select {
	case id := <-started:
		if id != 42 {
			t.Fatalf("expected worker started for server 42, got %d", id)
		}
	case <-time.After(time.Second):
		t.Fatal("worker never started")
	}
	if !a.WorkerManager.Running(42) {
		t.Fatal("expected worker to be running after ConnectServer")
	}
	if !a.consumeFirstConnect(42) {
		t.Fatal("expected ConnectServer to mark the server as a first connect")
	}
	a.WorkerManager.StopAll()
}

func TestConnectServerIsIdempotentWhileRunning(t *testing.T) {
	starts := make(chan int64, 4)
	a := &App{}
	a.WorkerManager = servers.NewWorkerManager(func(ctx context.Context, id int64) error {
		starts <- id
		<-ctx.Done()
		return nil
	})
	if err := a.ConnectServer(context.Background(), 7); err != nil {
		t.Fatalf("first connect failed: %v", err)
	}
	<-starts
	// Calling ConnectServer again while already running must not error or
	// start a duplicate worker.
	if err := a.ConnectServer(context.Background(), 7); err != nil {
		t.Fatalf("second connect while running should be a no-op, got: %v", err)
	}
	select {
	case <-starts:
		t.Fatal("expected no duplicate worker start while already running")
	case <-time.After(50 * time.Millisecond):
	}
	a.WorkerManager.StopAll()
}

func TestRepairServerDoesNotMarkFirstConnect(t *testing.T) {
	a := &App{}
	a.WorkerManager = servers.NewWorkerManager(func(ctx context.Context, id int64) error {
		<-ctx.Done()
		return nil
	})
	if err := a.RepairServer(context.Background(), 9); err != nil {
		t.Fatalf("RepairServer failed: %v", err)
	}
	if a.consumeFirstConnect(9) {
		t.Fatal("RepairServer must never trigger a tail-start (would skip missed activity)")
	}
	a.WorkerManager.StopAll()
}

func TestDisconnectServerStopsWorker(t *testing.T) {
	a := &App{}
	a.WorkerManager = servers.NewWorkerManager(func(ctx context.Context, id int64) error {
		<-ctx.Done()
		return nil
	})
	if err := a.ConnectServer(context.Background(), 3); err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	// Wait for the worker to actually register as running.
	deadline := time.Now().Add(time.Second)
	for !a.WorkerManager.Running(3) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !a.WorkerManager.Running(3) {
		t.Fatal("worker never reported running")
	}
	a.DisconnectServer(3)
	deadline = time.Now().Add(time.Second)
	for a.WorkerManager.Running(3) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if a.WorkerManager.Running(3) {
		t.Fatal("expected worker to stop after DisconnectServer")
	}
}

func TestNitradoStartupIsOptional(t *testing.T) {
	a := &App{Config: &config.Config{NitradoToken: ""}}
	if a.nitradoEnabled() {
		t.Fatal("expected Nitrado startup to be disabled when token is blank")
	}

	a = &App{Config: &config.Config{NitradoToken: "token"}}
	if !a.nitradoEnabled() {
		t.Fatal("expected Nitrado startup to be enabled when token is configured")
	}
}

func TestNitradoClientFromConnectionDecryptsGuildCredential(t *testing.T) {
	cipher, err := security.NewAESGCM("12345678901234567890123456789012", 1)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, version, err := cipher.Encrypt([]byte("guild-token"))
	if err != nil {
		t.Fatal(err)
	}

	client, err := nitradoClientFromConnection(cipher, repository.NitradoConnection{
		Ciphertext: ciphertext,
		Nonce:      nonce,
		KeyVersion: version,
	})
	if err != nil {
		t.Fatalf("expected encrypted credential to resolve: %v", err)
	}
	if client.BaseURL() != "https://api.nitrado.net" {
		t.Fatalf("unexpected client base URL: %q", client.BaseURL())
	}
}

func TestNitradoClientFromConnectionRejectsMissingCipher(t *testing.T) {
	if _, err := nitradoClientFromConnection(nil, repository.NitradoConnection{}); err == nil {
		t.Fatal("expected missing cipher to reject credential resolution")
	}
}

func TestBindOnlineCounterRefreshesSetupCreatedAfterStartup(t *testing.T) {
	store := discord.NewInMemorySetupStore()
	counter := discord.NewVoiceChannelCounter(nil, "")
	if err := store.Save(discord.GuildSetup{GuildID: "guild-1", OnlinePlayersChannelID: "voice-1"}); err != nil {
		t.Fatal(err)
	}

	bindLegacyOnlineCounter(store, "guild-1", counter)
	if counter.ChannelID() != "voice-1" {
		t.Fatalf("expected counter to bind setup channel, got %q", counter.ChannelID())
	}
}

func TestVerifyNitradoContinuesAfterUnauthorizedToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	a := &App{
		Config:  &config.Config{NitradoToken: "expired-token"},
		Nitrado: nitrado.NewClient(server.URL, "expired-token", server.Client()),
	}
	authenticated, verified, _, _, _ := a.verifyNitrado(context.Background())
	if authenticated || verified {
		t.Fatalf("expected unauthorized Nitrado token to remain degraded, got authenticated=%v verified=%v", authenticated, verified)
	}
}

// TestFeedDeliveryModeFromEnvironment: only KILLFEED_DELIVERY_MODE=immediate
// enables immediate delivery; unset (the production value) and anything else
// is the rotating cycle. runServerWorker applies this one value to both the
// killfeed and the deathfeed.
func TestFeedDeliveryModeFromEnvironment(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"", discord.FeedModeRotating},
		{"immediate", discord.FeedModeImmediate},
		{" IMMEDIATE ", discord.FeedModeImmediate},
		{"rotating", discord.FeedModeRotating},
		{"fast", discord.FeedModeRotating},
	} {
		t.Setenv("KILLFEED_DELIVERY_MODE", tc.env)
		if got := feedDeliveryMode(); got != tc.want {
			t.Errorf("KILLFEED_DELIVERY_MODE=%q: got %s, want %s", tc.env, got, tc.want)
		}
	}
}

func TestRuntimeBuildReportsDeploymentIdentity(t *testing.T) {
	t.Setenv("RAILWAY_GIT_COMMIT_SHA", "0123abc")
	t.Setenv("KILLFEED_DELIVERY_MODE", "immediate")
	a := &App{Config: &config.Config{AppEnv: "staging", NitradoAPIBaseURL: "http://fixture:8080"}}
	b := a.runtimeBuild()
	if b.Commit != "0123abc" || b.AppEnv != "staging" || b.KillfeedDeliveryMode != discord.FeedModeImmediate || b.NitradoSource != "fixture" {
		t.Fatalf("staging build %+v", b)
	}
	t.Setenv("KILLFEED_DELIVERY_MODE", "")
	a = &App{Config: &config.Config{AppEnv: "production"}}
	if b := a.runtimeBuild(); b.KillfeedDeliveryMode != discord.FeedModeRotating || b.NitradoSource != "nitrado" {
		t.Fatalf("production build %+v", b)
	}
}
