//go:build integration

package canaryops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The operator service against a real PostgreSQL: the real ledger (0054/0055), the real Shop refund
// and fulfil paths, and real organization membership. Nothing contacts a game server.

type tenant struct {
	org, inst, server, guild, player int64
	owner, admin, member             Actor
	session                          string
}

type world struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	shop *repository.ShopRepository
	a, b tenant
	seq  atomic.Int64
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newWorld(t *testing.T) *world {
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
	must(t, err)
	t.Cleanup(db.Close)
	must(t, db.Migrate(ctx))
	w := &world{t: t, ctx: ctx, pool: db.Pool, shop: repository.NewShopRepository(db.Pool)}
	w.a, w.b = w.tenant("a"), w.tenant("b")
	return w
}

func (w *world) one(sql string, args ...any) int64 {
	w.t.Helper()
	var id int64
	must(w.t, w.pool.QueryRow(w.ctx, sql, args...).Scan(&id))
	return id
}

func (w *world) tenant(name string) tenant {
	tag := fmt.Sprintf("%s%d", name, time.Now().UnixNano())
	user := func(role string) int64 {
		return w.one(`INSERT INTO app_users(discord_user_id, discord_username) VALUES($1,$2) RETURNING id`, fmt.Sprintf("%d%d", 7000+w.seq.Add(1), time.Now().UnixNano()%1e9), role)
	}
	var tn tenant
	ownerID, adminID, memberID := user("owner"), user("admin"), user("member")
	tn.org = w.one(`INSERT INTO organizations(name, slug, owner_user_id) VALUES('Canary Org',$1,$2) RETURNING id`, "canary-"+tag, ownerID)
	for id, role := range map[int64]string{ownerID: "OWNER", adminID: "ADMIN", memberID: "MEMBER"} {
		_, err := w.pool.Exec(w.ctx, `INSERT INTO organization_members(organization_id, user_id, role) VALUES($1,$2,$3)`, tn.org, id, role)
		must(w.t, err)
	}
	actor := func(id int64) Actor {
		var d string
		must(w.t, w.pool.QueryRow(w.ctx, `SELECT discord_user_id FROM app_users WHERE id=$1`, id).Scan(&d))
		return Actor{UserID: id, DiscordID: d}
	}
	tn.owner, tn.admin, tn.member = actor(ownerID), actor(adminID), actor(memberID)
	tn.guild = w.one(`INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, "g-"+tag)
	conn := w.one(`INSERT INTO discord_guild_connections(organization_id, guild_id, guild_name, bot_installed, permissions_verified) VALUES($1,$2,'G',TRUE,TRUE) RETURNING id`, tn.org, tn.guild)
	tn.server = w.one(`INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, display_name, status, organization_id)
		VALUES($1,'NITRADO',$2,'DAYZ','PLAYSTATION','Canary','ACTIVE',$3) RETURNING id`, tn.guild, fmt.Sprint(time.Now().UnixNano()%1e12), tn.org)
	tn.inst = w.one(`INSERT INTO installations(organization_id, discord_guild_connection_id, game_server_id, status) VALUES($1,$2,$3,'READY') RETURNING id`, tn.org, conn, tn.server)
	_, err := w.pool.Exec(w.ctx, `INSERT INTO shop_delivery_settings(installation_id, organization_id, map_key) VALUES($1,$2,'chernarusplus')`, tn.inst, tn.org)
	must(w.t, err)
	tn.session = fmt.Sprintf("dayzps/config/DayZServer_PS4_x64_%s.ADM", tag)
	_, err = w.pool.Exec(w.ctx, `INSERT INTO server_adm_sessions(server_id, guild_id, adm_file, session_local_start) VALUES($1,$2,$3,NOW()::timestamp)`, tn.server, tn.guild, tn.session)
	must(w.t, err)
	tn.player = w.one(`INSERT INTO players(guild_id, dayz_player_id, display_name) VALUES($1,$2,'Canary') RETURNING id`, tn.guild, "p-"+tag)
	return tn
}

// canaryDelivery inserts an open 1-point MANUAL_COORDINATE purchase, as the Shop purchase flow leaves it.
func (w *world) canaryDelivery(tn tenant) (purchase, delivery int64) {
	purchase = w.one(`INSERT INTO shop_purchases(organization_id, installation_id, game_server_id, player_id, status, total_points, delivery_type, idempotency_key, paid_at)
		VALUES($1,$2,$3,$4,'PENDING_FULFILLMENT',1,'MANUAL',$5,NOW()) RETURNING id`, tn.org, tn.inst, tn.server, tn.player, fmt.Sprintf("canary-%d-%d", time.Now().UnixNano(), w.seq.Add(1)))
	_, err := w.pool.Exec(w.ctx, `INSERT INTO shop_purchase_items(purchase_id, product_name, unit_price_points, quantity, line_total_points) VALUES($1,'Canary BandageDressing',1,1,1)`, purchase)
	must(w.t, err)
	delivery = w.one(`INSERT INTO shop_deliveries(purchase_id, organization_id, installation_id, game_server_id, player_id, delivery_policy, map_key, coord_x, coord_z, status)
		VALUES($1,$2,$3,$4,$5,'MANUAL_COORDINATE','chernarusplus',4621.1,8397.2,'MANUAL_READY') RETURNING id`, purchase, tn.org, tn.inst, tn.server, tn.player)
	return
}

func (w *world) service() *Service {
	return New(repository.NewShopAttemptRepository(w.pool), w.shop, repository.NewOrganizationRepository(w.pool),
		economy.NewAccounts(nil, repository.NewEconomyRepository(w.pool)), NewGate(true, []int64{w.a.inst})) // B stays locked
}

func (w *world) create(s *Service, tn tenant, delivery int64) (AttemptView, error) {
	return s.CreateAttempt(w.ctx, tn.org, tn.inst, tn.owner, CreateRequest{DeliveryID: delivery, AltitudeY: 319.6, DropSourceFile: tn.session,
		DropSourceOffset: 853, DropObservedAt: time.Now().Add(-2 * time.Minute)})
}

func (w *world) refund(tn tenant, purchase int64) error {
	_, err := w.shop.Refund(w.ctx, repository.RefundParams{OrganizationID: tn.org, InstallationID: tn.inst, GuildID: tn.guild, ServerID: tn.server,
		PurchaseID: purchase, ActorUserID: tn.owner.UserID, ActorDiscordID: tn.owner.DiscordID, Reason: "canary"})
	return err
}

func (w *world) manualFulfil(tn tenant, purchase int64) error {
	_, err := w.shop.Fulfill(w.ctx, tn.org, tn.inst, purchase, tn.owner.UserID)
	return err
}

func (w *world) attempts(tn tenant) int64 {
	return w.one(`SELECT COUNT(*) FROM shop_delivery_attempts WHERE installation_id=$1`, tn.inst)
}

func wantIs(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: got %v, want %v", what, err, want)
	}
}

func hex(c string) string { return strings.Repeat(c, 64) }

// stage drives an attempt to AWAITING_RESTART with recorded evidence; staged at `at`.
func (w *world) stage(s *Service, tn tenant, id string, at time.Time) {
	w.t.Helper()
	a := tn.admin
	_, err := s.AdvanceAttempt(w.ctx, tn.org, tn.inst, a, id, repository.AttemptPlanCreated, repository.AttemptFilePrepared, "")
	must(w.t, err)
	_, err = s.RecordEvidence(w.ctx, tn.org, tn.inst, a, id, EvidenceRequest{Kind: repository.EvidenceStagedFileHash, Source: repository.SourceNitradoReadback, SHA256: hex("5"), PreviousSHA256: hex("e"), ObservedAt: at})
	must(w.t, err)
	_, err = s.RecordEvidence(w.ctx, tn.org, tn.inst, a, id, EvidenceRequest{Kind: repository.EvidenceStagingBoot, Source: repository.SourceBootAuthority, BootFile: tn.session, ObservedAt: at})
	must(w.t, err)
	_, err = s.AdvanceAttempt(w.ctx, tn.org, tn.inst, a, id, repository.AttemptFilePrepared, repository.AttemptFileStaged, "")
	must(w.t, err)
	_, err = s.AdvanceAttempt(w.ctx, tn.org, tn.inst, a, id, repository.AttemptFileStaged, repository.AttemptAwaitingRestart, "")
	must(w.t, err)
}

func TestCanaryOperatorFullRun(t *testing.T) {
	w := newWorld(t)
	s := w.service()
	tn := w.a
	purchase, delivery := w.canaryDelivery(tn)
	v, err := w.create(s, tn, delivery)
	must(t, err)
	id := v.Attempt.AttemptID
	if id != fmt.Sprintf("champion:d%d:a1", delivery) || v.Attempt.PosY != 319.6 || v.Attempt.DropSourceFile != tn.session {
		t.Fatalf("%+v", v.Attempt)
	}
	now := time.Now()
	stagedAt := now.Add(-20 * time.Minute)
	w.stage(s, tn, id, stagedAt)

	// Item possibly on the server: no refund, no manual fulfilment.
	wantIs(t, "refund while staged", w.refund(tn, purchase), repository.ErrShopDeliveryAttemptActive)
	wantIs(t, "manual fulfil while staged", w.manualFulfil(tn, purchase), repository.ErrShopDeliveryAttemptActive)

	rec := func(r EvidenceRequest) error {
		_, err := s.RecordEvidence(w.ctx, tn.org, tn.inst, tn.admin, id, r)
		return err
	}
	adv := func(from, to string) error {
		_, err := s.AdvanceAttempt(w.ctx, tn.org, tn.inst, tn.owner, id, from, to, "")
		return err
	}
	// Evidence the state cannot have yet is refused by the database.
	secondAt := now.Add(-5 * time.Minute)
	wantIs(t, "second boot too early", rec(EvidenceRequest{Kind: repository.EvidenceSecondBoot, Source: repository.SourceBootAuthority, BootFile: "later.ADM", BootStartedAt: &secondAt, ObservedAt: now.Add(-4 * time.Minute)}), repository.ErrShopAttemptEvidenceState)
	// The log line is recorded, informationally.
	must(t, rec(EvidenceRequest{Kind: repository.EvidenceSpawnerLog, Source: repository.SourceRPTLog, ObservedAt: now.Add(-16 * time.Minute), Detail: "CE init reached, no [::SpawnObjects] error for the Champion file"}))
	bootAt := now.Add(-17 * time.Minute)
	must(t, rec(EvidenceRequest{Kind: repository.EvidenceNewBoot, Source: repository.SourceBootAuthority, BootFile: "dayzps/config/DayZServer_PS4_x64_new.ADM", BootStartedAt: &bootAt, ObservedAt: now.Add(-10 * time.Minute)}))
	must(t, adv(repository.AttemptAwaitingRestart, repository.AttemptRestartObserved))
	must(t, adv(repository.AttemptRestartObserved, repository.AttemptUnstageRequired))
	// Even bypassing the service, the database refuses a log line as physical proof (the state accepts
	// ITEM_OBSERVED here, so it is the source rule that refuses it).
	rowID := v.Attempt.ID
	if _, err := w.pool.Exec(w.ctx, `INSERT INTO shop_delivery_attempt_evidence(attempt_row_id, organization_id, installation_id, kind, source, recorded_by, observed_by, observed_at)
		VALUES($1,$2,$3,'ITEM_OBSERVED','RPT_LOG','x','x',NOW())`, rowID, tn.org, tn.inst); err == nil || !strings.Contains(err.Error(), "shop_attempt_evidence_source") {
		t.Fatalf("RPT as physical proof: %v", err)
	}
	must(t, rec(EvidenceRequest{Kind: repository.EvidenceItemObserved, Source: repository.SourceInGameObservation, ObservedBy: "owner-in-game", ObservedAt: now.Add(-15 * time.Minute)}))
	must(t, rec(EvidenceRequest{Kind: repository.EvidencePickupConfirmed, Source: repository.SourceInGameObservation, ObservedBy: "OwnerCharacter", ObservedAt: now.Add(-14 * time.Minute)}))
	wantIs(t, "evidence is write-once", rec(EvidenceRequest{Kind: repository.EvidenceItemObserved, Source: repository.SourceInGameObservation, ObservedBy: "someone", ObservedAt: now.Add(-13 * time.Minute)}), repository.ErrShopAttemptEvidenceExists)
	must(t, rec(EvidenceRequest{Kind: repository.EvidenceUnstagedFileHash, Source: repository.SourceNitradoReadback, SHA256: hex("e"), ObservedAt: now.Add(-9 * time.Minute)}))
	must(t, adv(repository.AttemptUnstageRequired, repository.AttemptVerificationRequired))
	// Fulfilment needs the second boot and the in-game no-respawn check; a clean RPT is not enough.
	_, err = s.FulfillAttempt(w.ctx, tn.org, tn.inst, tn.owner, id, "")
	wantIs(t, "fulfil without second boot", err, ErrMissingEvidence)
	must(t, rec(EvidenceRequest{Kind: repository.EvidenceSecondBoot, Source: repository.SourceBootAuthority, BootFile: "dayzps/config/DayZServer_PS4_x64_second.ADM", BootStartedAt: &secondAt, ObservedAt: now.Add(-4 * time.Minute)}))
	must(t, rec(EvidenceRequest{Kind: repository.EvidenceNoAdditionalSpawn, Source: repository.SourceInGameObservation, ObservedBy: "owner-in-game", ObservedAt: now.Add(-2 * time.Minute)}))
	v, err = s.FulfillAttempt(w.ctx, tn.org, tn.inst, tn.owner, id, "canary complete")
	must(t, err)
	if v.Attempt.State != repository.AttemptFulfilled || v.Attempt.PickedUpBy == nil || *v.Attempt.VerifiedBy != tn.owner.DiscordID {
		t.Fatalf("%+v", v.Attempt)
	}
	var ps, ds string
	must(t, w.pool.QueryRow(w.ctx, `SELECT sp.status, sd.status FROM shop_purchases sp JOIN shop_deliveries sd ON sd.purchase_id=sp.id WHERE sp.id=$1`, purchase).Scan(&ps, &ds))
	if ps != "FULFILLED" || ds != "FULFILLED" {
		t.Fatalf("%s %s", ps, ds)
	}
	// Every ledger fact came from recorded evidence, each with its source and actor.
	for _, e := range v.Evidence {
		if e.RecordedBy == "" || e.Source == "" {
			t.Fatalf("%+v", e)
		}
	}
	// Recorded evidence cannot be edited.
	if _, err := w.pool.Exec(w.ctx, `UPDATE shop_delivery_attempt_evidence SET observed_by='forged' WHERE attempt_row_id=$1`, v.Attempt.ID); err == nil {
		t.Fatal("evidence edited")
	}
}

func TestCanaryAuthorizationIsolationAndLock(t *testing.T) {
	w := newWorld(t)
	s := w.service()
	_, delivery := w.canaryDelivery(w.a)
	req := CreateRequest{DeliveryID: delivery, AltitudeY: 319.6, DropSourceFile: w.a.session, DropSourceOffset: 853, DropObservedAt: time.Now().Add(-time.Minute)}
	_, err := s.CreateAttempt(w.ctx, w.a.org, w.a.inst, w.a.member, req)
	wantIs(t, "member", err, ErrForbidden)
	_, err = s.CreateAttempt(w.ctx, w.a.org, w.a.inst, w.b.owner, req)
	wantIs(t, "other tenant's owner", err, ErrForbidden)
	_, err = s.CreateAttempt(w.ctx, w.a.org, w.b.inst, w.a.owner, req)
	wantIs(t, "another organization's installation", err, ErrInstallationUnknown)
	// B's installation is locked even for B's owner.
	_, bDelivery := w.canaryDelivery(w.b)
	_, err = s.CreateAttempt(w.ctx, w.b.org, w.b.inst, w.b.owner, CreateRequest{DeliveryID: bDelivery, AltitudeY: 1, DropSourceFile: w.b.session, DropSourceOffset: 1, DropObservedAt: time.Now()})
	wantIs(t, "locked installation", err, ErrExecutionLocked)
	if w.attempts(w.b) != 0 || w.attempts(w.a) != 0 {
		t.Fatal("a refused request wrote an attempt")
	}
	// A's admin may; B's delivery id under A's scope is not found.
	_, err = s.CreateAttempt(w.ctx, w.a.org, w.a.inst, w.a.owner, CreateRequest{DeliveryID: bDelivery, AltitudeY: 1, DropSourceFile: w.a.session, DropSourceOffset: 1, DropObservedAt: time.Now()})
	wantIs(t, "other tenant's delivery", err, repository.ErrShopDeliveryNotFound)
	v, err := s.CreateAttempt(w.ctx, w.a.org, w.a.inst, w.a.admin, req)
	must(t, err)
	// Reads are tenant-scoped; B's owner cannot read A's attempt through B's installation.
	_, err = s.GetAttempt(w.ctx, w.b.org, w.b.inst, w.b.owner, v.Attempt.AttemptID)
	wantIs(t, "cross-tenant read", err, repository.ErrShopAttemptNotFound)
	list, err := s.ListAttempts(w.ctx, w.b.org, w.b.inst, w.b.owner, false, 50)
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("B sees %d attempts", len(list))
	}
	// Invalid transitions go through the ledger's state machine.
	_, err = s.AdvanceAttempt(w.ctx, w.a.org, w.a.inst, w.a.owner, v.Attempt.AttemptID, repository.AttemptPlanCreated, repository.AttemptAwaitingRestart, "")
	wantIs(t, "skipping states", err, repository.ErrShopAttemptRejected)
	_, err = s.AdvanceAttempt(w.ctx, w.a.org, w.a.inst, w.a.owner, v.Attempt.AttemptID, repository.AttemptFilePrepared, repository.AttemptFileStaged, "")
	wantIs(t, "missing evidence names itself", err, ErrMissingEvidence)
	// A second attempt while one is open, and a drop point from another boot, are refused.
	_, err = w.create(s, w.a, delivery)
	wantIs(t, "duplicate attempt", err, repository.ErrShopAttemptConflict)
	_, err = s.CreateAttempt(w.ctx, w.a.org, w.a.inst, w.a.owner, CreateRequest{DeliveryID: delivery, AltitudeY: 1, DropSourceFile: w.b.session, DropSourceOffset: 1, DropObservedAt: time.Now()})
	wantIs(t, "drop point from another boot", err, ErrInvalid)
}

func TestCanaryConcurrency(t *testing.T) {
	w := newWorld(t)
	s := w.service()
	_, delivery := w.canaryDelivery(w.a)
	v, err := w.create(s, w.a, delivery)
	must(t, err)
	id := v.Attempt.AttemptID
	var ok, stale atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.AdvanceAttempt(w.ctx, w.a.org, w.a.inst, w.a.owner, id, repository.AttemptPlanCreated, repository.AttemptFilePrepared, "")
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, repository.ErrShopAttemptStale):
				stale.Add(1)
			default:
				t.Errorf("advance: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || stale.Load() != 15 {
		t.Fatalf("concurrent advance: %d ok %d stale", ok.Load(), stale.Load())
	}
	var recorded, dup atomic.Int64
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.RecordEvidence(w.ctx, w.a.org, w.a.inst, w.a.admin, id, EvidenceRequest{Kind: repository.EvidenceStagedFileHash, Source: repository.SourceNitradoReadback,
				SHA256: hex(fmt.Sprint(i % 10)), PreviousSHA256: hex("e"), ObservedAt: time.Now().Add(-time.Minute)})
			switch {
			case err == nil:
				recorded.Add(1)
			case errors.Is(err, repository.ErrShopAttemptEvidenceExists), errors.Is(err, repository.ErrShopAttemptEvidence):
				dup.Add(1) // duplicate kind, or the one worker that drew sha "eee..." (= previous)
			default:
				t.Errorf("evidence: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if recorded.Load() != 1 {
		t.Fatalf("write-once evidence under concurrency: %d recorded", recorded.Load())
	}
}

func TestCanaryReviewResolutionAndRecovery(t *testing.T) {
	w := newWorld(t)
	s := w.service()
	tn := w.a
	review := func(outcome string) (int64, string) {
		purchase, delivery := w.canaryDelivery(tn)
		v, err := w.create(s, tn, delivery)
		must(t, err)
		id := v.Attempt.AttemptID
		w.stage(s, tn, id, time.Now().Add(-30*time.Minute))
		// A second boot happened before the unstage was verified.
		_, err = s.AdvanceAttempt(w.ctx, tn.org, tn.inst, tn.owner, id, repository.AttemptAwaitingRestart, repository.AttemptFailedReview, "second boot before a verified unstage")
		must(t, err)
		return purchase, id
	}

	// UNCERTAIN: recorded, still blocked everywhere, no new attempt.
	p1, id1 := review(OutcomeUncertain)
	before := w.attempts(tn)
	v, err := s.ResolveReview(w.ctx, tn.org, tn.inst, tn.admin, id1, OutcomeUncertain, "the player is offline; cannot confirm either way")
	must(t, err)
	if v.Attempt.ReviewResolution != nil || len(v.Evidence) == 0 || v.Evidence[len(v.Evidence)-1].Kind != repository.EvidenceReviewUncertain {
		t.Fatalf("%+v", v)
	}
	wantIs(t, "refund while uncertain", w.refund(tn, p1), repository.ErrShopDeliveryAttemptActive)
	wantIs(t, "manual fulfil while uncertain", w.manualFulfil(tn, p1), repository.ErrShopDeliveryAttemptActive)
	if w.attempts(tn) != before {
		t.Fatal("a review outcome created an attempt")
	}
	// Then NOT_SPAWNED: the refund becomes possible; still no new attempt, ever.
	_, err = s.ResolveReview(w.ctx, tn.org, tn.inst, tn.owner, id1, OutcomeNotSpawned, "checked in game: nothing at the drop point, player has no bandage")
	must(t, err)
	_, err = s.ResolveReview(w.ctx, tn.org, tn.inst, tn.owner, id1, OutcomeUncertain, "late note")
	wantIs(t, "assessment after resolution", err, repository.ErrShopAttemptEvidenceState)
	var d1 int64
	must(t, w.pool.QueryRow(w.ctx, `SELECT delivery_id FROM shop_delivery_attempts WHERE attempt_id=$1`, id1).Scan(&d1))
	_, err = w.create(s, tn, d1)
	wantIs(t, "retry after review", err, repository.ErrShopAttemptConflict)
	must(t, w.refund(tn, p1))

	// SPAWNED: never refunded automatically; the manual fulfilment records it.
	p2, id2 := review(OutcomeSpawned)
	_, err = s.ResolveReview(w.ctx, tn.org, tn.inst, tn.owner, id2, OutcomeSpawned, "player confirmed the bandage in inventory")
	must(t, err)
	wantIs(t, "refund after SPAWNED", w.refund(tn, p2), repository.ErrShopDeliveryAttemptActive)
	must(t, w.manualFulfil(tn, p2))
	_, err = s.ResolveReview(w.ctx, tn.org, tn.inst, tn.owner, id2, OutcomeNotSpawned, "changing my mind")
	wantIs(t, "second resolution", err, repository.ErrShopAttemptStale)

	// Recovery: a worker/operator crash after FILE_PREPARED stays listed; abandoning it re-opens the refund.
	p3, d3 := w.canaryDelivery(tn)
	v3, err := w.create(s, tn, d3)
	must(t, err)
	_, err = s.AdvanceAttempt(w.ctx, tn.org, tn.inst, tn.owner, v3.Attempt.AttemptID, repository.AttemptPlanCreated, repository.AttemptFilePrepared, "")
	must(t, err)
	open, err := s.ListAttempts(w.ctx, tn.org, tn.inst, tn.owner, true, 50)
	must(t, err)
	found := false
	for _, a := range open {
		found = found || a.AttemptID == v3.Attempt.AttemptID
	}
	if !found {
		t.Fatal("the in-flight attempt must be listed for reconciliation")
	}
	wantIs(t, "refund with upload in flight", w.refund(tn, p3), repository.ErrShopDeliveryAttemptActive)
	_, err = s.AdvanceAttempt(w.ctx, tn.org, tn.inst, tn.owner, v3.Attempt.AttemptID, repository.AttemptFilePrepared, repository.AttemptAbandoned, " ")
	wantIs(t, "abandon without reason", err, ErrInvalid)
	_, err = s.AdvanceAttempt(w.ctx, tn.org, tn.inst, tn.owner, v3.Attempt.AttemptID, repository.AttemptFilePrepared, repository.AttemptAbandoned, "read-back: the file was never written")
	must(t, err)
	must(t, w.refund(tn, p3))
}
