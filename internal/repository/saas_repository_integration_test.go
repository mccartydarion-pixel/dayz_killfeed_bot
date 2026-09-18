//go:build integration

package repository

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
)

// saasIntegrationDB connects and migrates, following the same
// TEST_DATABASE_URL/ALLOW_INTEGRATION_DB_TESTS pattern as every other
// integration test in this repository (see database_integration_test.go).
func saasIntegrationDB(t *testing.T) *database.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required for integration suite")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for an explicit non-production integration database")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestAppUserDiscordIDCannotDuplicate is case A: the same Discord user must
// never produce two app_users rows - UpsertDiscordUser is a true upsert, and
// discord_user_id stays unique even under a raw duplicate insert attempt.
func TestAppUserDiscordIDCannotDuplicate(t *testing.T) {
	db := saasIntegrationDB(t)
	ctx := context.Background()
	users := NewUserRepository(db.Pool)

	suffix := time.Now().UnixNano()
	discordID := fmt.Sprintf("saas-user-%d", suffix)

	first, err := users.UpsertDiscordUser(ctx, AppUser{DiscordUserID: discordID, DiscordUsername: "Original"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := users.UpsertDiscordUser(ctx, AppUser{DiscordUserID: discordID, DiscordUsername: "Renamed"})
	if err != nil {
		t.Fatalf("expected upsert to update, not error, on a repeated discord_user_id: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected the same app_users row, got ids %d and %d", first.ID, second.ID)
	}
	if second.DiscordUsername != "Renamed" {
		t.Fatalf("expected the upsert to update the username, got %q", second.DiscordUsername)
	}

	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM app_users WHERE discord_user_id=$1`, discordID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one app_users row for this discord_user_id, got %d", count)
	}

	// A raw duplicate INSERT (bypassing the upsert path) must still be
	// rejected by the unique constraint itself.
	_, err = db.Pool.Exec(ctx, `INSERT INTO app_users(discord_user_id, discord_username) VALUES($1,'Duplicate')`, discordID)
	if !isUniqueViolation(err) {
		t.Fatalf("expected a unique constraint violation for a duplicate discord_user_id, got %v", err)
	}
}

// TestOrganizationCreateSafelyCreatesOwnerMembership is case B: creating an
// organization must always leave it with exactly one OWNER membership row,
// created atomically with the organization itself.
func TestOrganizationCreateSafelyCreatesOwnerMembership(t *testing.T) {
	db := saasIntegrationDB(t)
	ctx := context.Background()
	users := NewUserRepository(db.Pool)
	orgs := NewOrganizationRepository(db.Pool)

	suffix := time.Now().UnixNano()
	owner, err := users.UpsertDiscordUser(ctx, AppUser{DiscordUserID: fmt.Sprintf("saas-owner-%d", suffix), DiscordUsername: "Owner"})
	if err != nil {
		t.Fatal(err)
	}

	org, err := orgs.Create(ctx, "Test Org", fmt.Sprintf("test-org-%d", suffix), owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if org.OwnerUserID != owner.ID {
		t.Fatalf("expected owner_user_id %d, got %d", owner.ID, org.OwnerUserID)
	}

	role, ok, err := orgs.VerifyMembership(ctx, org.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected the owner to be a member immediately after Create")
	}
	if role != RoleOwner {
		t.Fatalf("expected role %q, got %q", RoleOwner, role)
	}

	var memberCount int
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM organization_members WHERE organization_id=$1`, org.ID).Scan(&memberCount); err != nil {
		t.Fatal(err)
	}
	if memberCount != 1 {
		t.Fatalf("expected exactly one membership row, got %d", memberCount)
	}
}

// saasFixture builds one organization (with owner), a claimed guild
// connection, and an installation - the common scaffold most of the
// tenant-isolation tests below need. It reuses the existing guilds/
// game_servers tables via GuildRepository/ServerRepository, matching how a
// real installation is actually built up (guild already connected via the
// bot, server already connected via Nitrado, THEN claimed by an
// organization).
type saasFixture struct {
	OrgID          int64
	OwnerUserID    int64
	GuildRowID     int64
	ServerRowID    int64
	ConnectionID   int64
	InstallationID int64
}

func newSaaSFixture(t *testing.T, db *database.DB) saasFixture {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	users := NewUserRepository(db.Pool)
	orgs := NewOrganizationRepository(db.Pool)
	guilds := NewGuildRepository(db.Pool)
	servers := NewServerRepository(db.Pool)
	guildConns := NewGuildConnectionRepository(db.Pool)
	saasServers := NewSaaSServerRepository(db.Pool)
	installations := NewInstallationRepository(db.Pool)

	owner, err := users.UpsertDiscordUser(ctx, AppUser{DiscordUserID: fmt.Sprintf("saas-fixture-user-%d", suffix), DiscordUsername: "Fixture"})
	if err != nil {
		t.Fatal(err)
	}
	org, err := orgs.Create(ctx, "Fixture Org", fmt.Sprintf("fixture-org-%d", suffix), owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	guildRowID, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("saas-fixture-guild-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	server, err := servers.UpsertGameServer(ctx, GameServer{GuildID: guildRowID, Provider: "NITRADO", ProviderServiceID: fmt.Sprintf("saas-svc-%d", suffix), Game: "DayZ", Platform: "PLAYSTATION", DisplayName: "Fixture Server", Status: "CONNECTED", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := guildConns.Upsert(ctx, DiscordGuildConnection{OrganizationID: org.ID, GuildID: guildRowID, GuildName: "Fixture Guild", BotInstalled: true, PermissionsVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := saasServers.ClaimForOrganization(ctx, org.ID, guildRowID, server.ProviderServiceID); err != nil {
		t.Fatal(err)
	}
	install, err := installations.Create(ctx, org.ID, conn.ID, &server.ID)
	if err != nil {
		t.Fatal(err)
	}

	return saasFixture{OrgID: org.ID, OwnerUserID: owner.ID, GuildRowID: guildRowID, ServerRowID: server.ID, ConnectionID: conn.ID, InstallationID: install.ID}
}

// TestInstallationTenantIsolation is cases C/D/E/F: organization A must
// never resolve organization B's installation, guild connection, or DayZ
// server through ID guessing - every scoped lookup requires the correct
// organization ID.
func TestInstallationTenantIsolation(t *testing.T) {
	db := saasIntegrationDB(t)
	ctx := context.Background()
	fixtureA := newSaaSFixture(t, db)
	fixtureB := newSaaSFixture(t, db)

	installations := NewInstallationRepository(db.Pool)
	guildConns := NewGuildConnectionRepository(db.Pool)
	saasServers := NewSaaSServerRepository(db.Pool)

	// D: guild connection is tenant scoped.
	if got, err := guildConns.GetScoped(ctx, fixtureA.OrgID, fixtureB.ConnectionID); err != nil {
		t.Fatal(err)
	} else if got != nil {
		t.Fatal("expected organization A to never resolve organization B's guild connection")
	}
	if got, err := guildConns.GetScoped(ctx, fixtureB.OrgID, fixtureB.ConnectionID); err != nil {
		t.Fatal(err)
	} else if got == nil {
		t.Fatal("expected organization B to resolve its own guild connection")
	}

	// E: DayZ server connection is tenant scoped.
	if got, err := saasServers.GetScoped(ctx, fixtureA.OrgID, fixtureB.ServerRowID); err != nil {
		t.Fatal(err)
	} else if got != nil {
		t.Fatal("expected organization A to never resolve organization B's server")
	}
	if got, err := saasServers.GetScoped(ctx, fixtureB.OrgID, fixtureB.ServerRowID); err != nil {
		t.Fatal(err)
	} else if got == nil {
		t.Fatal("expected organization B to resolve its own server")
	}

	// C/F: installation is tenant scoped.
	if got, err := installations.GetScoped(ctx, fixtureA.OrgID, fixtureB.InstallationID); err != nil {
		t.Fatal(err)
	} else if got != nil {
		t.Fatal("expected organization A to never resolve organization B's installation")
	}
	if got, err := installations.GetScoped(ctx, fixtureB.OrgID, fixtureB.InstallationID); err != nil {
		t.Fatal(err)
	} else if got == nil {
		t.Fatal("expected organization B to resolve its own installation")
	}
}

// TestCredentialEnvelopePersistsWithoutPlaintext is case G: only the
// encrypted envelope (ciphertext/nonce/key_version) ever round-trips through
// CredentialRepository - never a plaintext token - and it stays tenant
// scoped like every other SaaS lookup.
func TestCredentialEnvelopePersistsWithoutPlaintext(t *testing.T) {
	db := saasIntegrationDB(t)
	ctx := context.Background()
	fixture := newSaaSFixture(t, db)
	creds := NewCredentialRepository(db.Pool)

	ciphertext := make([]byte, 64)
	nonce := make([]byte, 12)
	if _, err := rand.Read(ciphertext); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}

	if err := creds.InsertForOrganization(ctx, fixture.GuildRowID, CredentialEnvelope{
		OrganizationID: fixture.OrgID,
		Ciphertext:     ciphertext,
		Nonce:          nonce,
		KeyVersion:     1,
		Status:         "ACTIVE",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := creds.GetScoped(ctx, fixture.OrgID, fixture.GuildRowID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected the credential envelope to be persisted")
	}
	if string(got.Ciphertext) != string(ciphertext) || string(got.Nonce) != string(nonce) {
		t.Fatal("expected the exact encrypted envelope to round-trip")
	}

	// Wrong organization must never resolve it (tenant isolation applies to
	// credentials too - section 15 explicitly lists them).
	otherFixture := newSaaSFixture(t, db)
	if got, err := creds.GetScoped(ctx, otherFixture.OrgID, fixture.GuildRowID); err != nil {
		t.Fatal(err)
	} else if got != nil {
		t.Fatal("expected a different organization to never resolve this credential envelope")
	}

	// Column check: nothing on nitrado_connections stores a plaintext token
	// column - the only new column this migration added is organization_id.
	var plaintextColumnCount int
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_name='nitrado_connections' AND column_name ILIKE '%plaintext%'`).Scan(&plaintextColumnCount); err != nil {
		t.Fatal(err)
	}
	if plaintextColumnCount != 0 {
		t.Fatal("expected no plaintext-shaped column on nitrado_connections")
	}
}

// TestSetupProgressSurvivesRestart is case H: setup progress is durable
// database state, not in-memory UI state - a fresh repository instance
// (simulating a process restart) must read back exactly what was written.
func TestSetupProgressSurvivesRestart(t *testing.T) {
	db := saasIntegrationDB(t)
	ctx := context.Background()
	fixture := newSaaSFixture(t, db)

	writer := NewInstallationRepository(db.Pool)
	progress := InstallationSetupProgress{
		CurrentStep:         "NITRADO",
		DiscordCompleted:    true,
		NitradoCompleted:    true,
		ServerSelected:      true,
		ChannelsCompleted:   false,
		ValidationCompleted: false,
	}
	if err := writer.UpdateSetupProgress(ctx, fixture.OrgID, fixture.InstallationID, progress); err != nil {
		t.Fatal(err)
	}

	// A brand-new repository value over the same pool stands in for "the
	// process restarted" - there is no in-process state to fall back on.
	reader := NewInstallationRepository(db.Pool)
	got, err := reader.GetSetupProgress(ctx, fixture.OrgID, fixture.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected setup progress to exist")
	}
	if got.CurrentStep != "NITRADO" || !got.DiscordCompleted || !got.NitradoCompleted || !got.ServerSelected || got.ChannelsCompleted || got.ValidationCompleted {
		t.Fatalf("expected progress to survive restart exactly as written, got %+v", got)
	}

	// Completing validation must stamp completed_at exactly once.
	progress.ChannelsCompleted = true
	progress.ValidationCompleted = true
	if err := writer.UpdateSetupProgress(ctx, fixture.OrgID, fixture.InstallationID, progress); err != nil {
		t.Fatal(err)
	}
	got, err = reader.GetSetupProgress(ctx, fixture.OrgID, fixture.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CompletedAt == nil {
		t.Fatal("expected completed_at to be stamped once validation completed")
	}
}

// TestInstallationStatusTransitionsPersist is case I: installation status
// transitions are durable, tenant-scoped, and stamp setup_completed_at
// exactly once, the first time status reaches READY.
func TestInstallationStatusTransitionsPersist(t *testing.T) {
	db := saasIntegrationDB(t)
	ctx := context.Background()
	fixture := newSaaSFixture(t, db)
	installations := NewInstallationRepository(db.Pool)

	for _, status := range []string{InstallationDiscordConnected, InstallationNitradoConnected, InstallationConfiguring} {
		if err := installations.UpdateStatus(ctx, fixture.OrgID, fixture.InstallationID, status); err != nil {
			t.Fatal(err)
		}
		got, err := installations.GetScoped(ctx, fixture.OrgID, fixture.InstallationID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != status {
			t.Fatalf("expected status %q to persist, got %q", status, got.Status)
		}
		if got.SetupCompletedAt != nil {
			t.Fatalf("expected setup_completed_at to stay unset before READY, got %v", got.SetupCompletedAt)
		}
	}

	if err := installations.UpdateStatus(ctx, fixture.OrgID, fixture.InstallationID, InstallationReady); err != nil {
		t.Fatal(err)
	}
	got, err := installations.GetScoped(ctx, fixture.OrgID, fixture.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != InstallationReady {
		t.Fatalf("expected status READY, got %q", got.Status)
	}
	if got.SetupCompletedAt == nil {
		t.Fatal("expected setup_completed_at to be stamped once status reached READY")
	}
	firstCompletedAt := *got.SetupCompletedAt

	// A later transition away from and back to READY must not re-stamp it.
	if err := installations.UpdateStatus(ctx, fixture.OrgID, fixture.InstallationID, InstallationDegraded); err != nil {
		t.Fatal(err)
	}
	if err := installations.UpdateStatus(ctx, fixture.OrgID, fixture.InstallationID, InstallationReady); err != nil {
		t.Fatal(err)
	}
	got, err = installations.GetScoped(ctx, fixture.OrgID, fixture.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.SetupCompletedAt.Equal(firstCompletedAt) {
		t.Fatalf("expected setup_completed_at to stay stamped at its first value, got %v then %v", firstCompletedAt, got.SetupCompletedAt)
	}
}

// TestSubscriptionEnsureTrialIsIdempotent proves the subscription foundation
// resolves to exactly one row per organization even if EnsureTrial is called
// more than once (e.g. a retried request).
func TestSubscriptionEnsureTrialIsIdempotent(t *testing.T) {
	db := saasIntegrationDB(t)
	ctx := context.Background()
	fixture := newSaaSFixture(t, db)
	subs := NewSubscriptionRepository(db.Pool)

	trialEnd := time.Now().Add(14 * 24 * time.Hour)
	first, err := subs.EnsureTrial(ctx, fixture.OrgID, trialEnd)
	if err != nil {
		t.Fatal(err)
	}
	second, err := subs.EnsureTrial(ctx, fixture.OrgID, trialEnd)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected EnsureTrial to be idempotent, got ids %d and %d", first.ID, second.ID)
	}

	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM subscriptions WHERE organization_id=$1`, fixture.OrgID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one subscription row, got %d", count)
	}

	got, err := subs.GetForOrganization(ctx, fixture.OrgID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Status != SubscriptionTrial {
		t.Fatalf("expected a TRIAL subscription, got %+v", got)
	}
}
