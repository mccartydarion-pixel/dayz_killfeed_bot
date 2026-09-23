//go:build integration

package factionstats

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/factionhub"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Real-PostgreSQL fixture for the Faction Hub competitive layer. Everything is created with unique
// names and removed by t.Cleanup, against the throwaway integration database only
// (TEST_DATABASE_URL + ALLOW_INTEGRATION_DB_TESTS=true). Real kills/deaths/bounties are inserted
// into the real tables with the same columns the killfeed writes.

type world struct {
	t     *testing.T
	ctx   context.Context
	db    *database.DB
	hub   *repository.FactionHubRepository
	store *repository.HubStatsRepository
	svc   *Service

	org1, inst1, inst1b int64 // one organization, two installations: the same guild, two DayZ servers
	org2, inst2         int64 // another organization, its own guild and server
	guild1, guild2      int64
	server1a, server1b  int64 // inst1 -> server1a, inst1b -> server1b (both in guild1)
	server2             int64
	suffix              int64
	n                   int
	base                time.Time // "30 days ago": timeline anchor
	userIDs             []int64   // every app_users row created, so cleanup can delete by id instead of LIKE
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
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, ctx: ctx, db: db, hub: repository.NewFactionHubRepository(db.Pool), store: repository.NewHubStatsRepository(db.Pool),
		suffix: time.Now().UnixNano(), base: time.Now().UTC().Add(-30 * 24 * time.Hour)}
	w.svc = NewService(w.store, Options{CacheTTL: time.Hour})

	owner1, owner2 := w.user(), w.user()
	w.org1 = w.one(`INSERT INTO organizations(name, slug, owner_user_id) VALUES('Stats Org A',$1,$2) RETURNING id`, fmt.Sprintf("st-%d-a", w.suffix), owner1)
	w.org2 = w.one(`INSERT INTO organizations(name, slug, owner_user_id) VALUES('Stats Org B',$1,$2) RETURNING id`, fmt.Sprintf("st-%d-b", w.suffix), owner2)
	w.guild1 = w.one(`INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, fmt.Sprintf("st-%d-g1", w.suffix))
	w.guild2 = w.one(`INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, fmt.Sprintf("st-%d-g2", w.suffix))
	conn1 := w.one(`INSERT INTO discord_guild_connections(organization_id, guild_id) VALUES($1,$2) RETURNING id`, w.org1, w.guild1)
	conn2 := w.one(`INSERT INTO discord_guild_connections(organization_id, guild_id) VALUES($1,$2) RETURNING id`, w.org2, w.guild2)
	w.server1a = w.server(w.guild1, "s1a")
	w.server1b = w.server(w.guild1, "s1b")
	w.server2 = w.server(w.guild2, "s2")
	w.inst1 = w.one(`INSERT INTO installations(organization_id, discord_guild_connection_id, game_server_id, status) VALUES($1,$2,$3,'READY') RETURNING id`, w.org1, conn1, w.server1a)
	w.inst1b = w.one(`INSERT INTO installations(organization_id, discord_guild_connection_id, game_server_id, status) VALUES($1,$2,$3,'READY') RETURNING id`, w.org1, conn1, w.server1b)
	w.inst2 = w.one(`INSERT INTO installations(organization_id, discord_guild_connection_id, game_server_id, status) VALUES($1,$2,$3,'READY') RETURNING id`, w.org2, conn2, w.server2)

	t.Cleanup(func() {
		for _, org := range []int64{w.org1, w.org2} {
			_, _ = db.Pool.Exec(ctx, `DELETE FROM organizations WHERE id=$1`, org)
		}
		// By id, not `discord_guild_id LIKE 'st-<suffix>-%'`: under a non-C collation LIKE
		// can't use the plain unique-constraint btree index, so it seq-scans the whole
		// table (shared with internal/repository's hubWorld) on every cleanup - on a
		// long-lived, never-truncated dev DB that scan alone can dominate the test.
		_, _ = db.Pool.Exec(ctx, `DELETE FROM guilds WHERE id = ANY($1)`, []int64{w.guild1, w.guild2})
		_, _ = db.Pool.Exec(ctx, `DELETE FROM app_users WHERE id = ANY($1)`, w.userIDs)
	})
	return w
}

func (w *world) one(sql string, args ...any) int64 {
	w.t.Helper()
	var id int64
	if err := w.db.Pool.QueryRow(w.ctx, sql, args...).Scan(&id); err != nil {
		w.t.Fatalf("fixture %q: %v", sql, err)
	}
	return id
}

func (w *world) exec(sql string, args ...any) {
	w.t.Helper()
	if _, err := w.db.Pool.Exec(w.ctx, sql, args...); err != nil {
		w.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (w *world) next() int { w.n++; return w.n }

func (w *world) server(guild int64, tag string) int64 {
	return w.one(`INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, status) VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE') RETURNING id`, guild, fmt.Sprintf("st-%d-%s", w.suffix, tag))
}

// user creates a website user (no DayZ identity).
func (w *world) user() int64 {
	n := w.next()
	id := w.one(`INSERT INTO app_users(discord_user_id, discord_username, discord_global_name) VALUES($1,$2,$3) RETURNING id`,
		fmt.Sprintf("st-%d-u%d", w.suffix, n), fmt.Sprintf("user%d", n), fmt.Sprintf("User %d", n))
	w.userIDs = append(w.userIDs, id)
	return id
}

func (w *world) discordID(user int64) string {
	var d string
	if err := w.db.Pool.QueryRow(w.ctx, `SELECT discord_user_id FROM app_users WHERE id=$1`, user).Scan(&d); err != nil {
		w.t.Fatal(err)
	}
	return d
}

// player creates a DayZ player identity (as the killfeed does).
func (w *world) player(guild int64, dzid, name string) int64 {
	return w.one(`INSERT INTO players(guild_id, dayz_player_id, display_name) VALUES($1,$2,$3) RETURNING id`, guild, dzid, name)
}

// link verifies user <-> player on guild (a VERIFIED player_links row).
func (w *world) link(guild, player, user int64, status string) {
	w.exec(`INSERT INTO player_links(guild_id, player_id, discord_user_id, status) VALUES($1,$2,$3,$4)`, guild, player, w.discordID(user), status)
}

// linked returns a new website user linked (VERIFIED) to a new player on guild.
func (w *world) linked(guild int64, name string) (user, player int64) {
	user = w.user()
	player = w.player(guild, fmt.Sprintf("dz-%d-%d", w.suffix, w.next()), name)
	w.link(guild, player, user, "VERIFIED")
	return
}

// outsider is a player with no website user at all (the ordinary victims).
func (w *world) outsider(guild int64, name string) int64 {
	return w.player(guild, fmt.Sprintf("dz-%d-%d", w.suffix, w.next()), name)
}

type killOpt struct {
	headshot, longshot bool
	distance           float64
	weapon             string
}

func (w *world) kill(guild, server, killer, victim int64, at time.Time, o killOpt) int64 {
	if o.weapon == "" {
		o.weapon = "M4-A1"
	}
	return w.one(`INSERT INTO kills(guild_id, server_id, session_id, event_fingerprint, killer_player_id, victim_player_id, headshot, longshot, distance, weapon_display, event_time)
VALUES($1,$2,'sess',$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`, guild, server, fmt.Sprintf("k-%d-%d", w.suffix, w.next()), killer, victim, o.headshot, o.longshot, o.distance, o.weapon, at)
}

func (w *world) death(guild, server, player int64, at time.Time, typ string) int64 {
	return w.one(`INSERT INTO deaths(guild_id, server_id, session_id, event_fingerprint, player_id, death_type, event_time) VALUES($1,$2,'sess',$3,$4,$5,$6) RETURNING id`,
		guild, server, fmt.Sprintf("d-%d-%d", w.suffix, w.next()), player, typ, at)
}

// bounty inserts a bounty CLAIMED by hunter with the given kill (a real claimed-bounty row).
func (w *world) bounty(guild, server, target, hunter, killID int64, reward int, at time.Time) int64 {
	return w.one(`INSERT INTO bounties(guild_id, server_id, target_player_id, created_by_type, status, reward_points, starts_at, claimed_by_player_id, claimed_kill_id, claimed_at)
VALUES($1,$2,$3,'MANUAL','CLAIMED',$4,$5,$6,$7,$8) RETURNING id`, guild, server, target, reward, at.Add(-time.Hour), hunter, killID, at)
}

// faction creates a faction on inst (org/guild inferred) led by leader.
func (w *world) faction(inst, leader int64, name, tag string) *repository.HubFaction {
	w.t.Helper()
	org := w.org1
	if inst == w.inst2 {
		org = w.org2
	}
	f, err := w.hub.CreateFaction(w.ctx, org, inst, leader, repository.HubFactionInput{Name: name, Tag: tag, RecruitmentStatus: "OPEN"})
	if err != nil {
		w.t.Fatalf("create faction %s: %v", name, err)
	}
	return f
}

// join adds user to the faction through the real apply + accept flow.
func (w *world) join(f *repository.HubFaction, leader, user int64) int64 {
	w.t.Helper()
	app, err := w.hub.Apply(w.ctx, f.OrganizationID, f.InstallationID, f.ID, user, "")
	if err != nil {
		w.t.Fatalf("apply: %v", err)
	}
	if _, m, err := w.hub.AcceptApplication(w.ctx, f.OrganizationID, f.InstallationID, f.ID, app.ID, leader); err != nil {
		w.t.Fatalf("accept: %v", err)
	} else {
		return m.ID
	}
	return 0
}

// span is one membership period; a zero To means still open (a current member).
type span struct{ From, To time.Time }

// periods replaces the user's membership history in the faction with exactly these periods (the live
// membership row is untouched). A zero To must correspond to a current member.
func (w *world) periods(f *repository.HubFaction, user int64, spans ...span) {
	w.t.Helper()
	w.exec(`DELETE FROM hub_faction_membership_history WHERE faction_id=$1 AND user_id=$2`, f.ID, user)
	for _, s := range spans {
		var to any
		if !s.To.IsZero() {
			to = s.To
		}
		w.exec(`INSERT INTO hub_faction_membership_history(organization_id, installation_id, faction_id, user_id, joined_at, left_at) VALUES($1,$2,$3,$4,$5,$6)`,
			f.OrganizationID, f.InstallationID, f.ID, user, s.From, to)
	}
}

func (w *world) day(n float64) time.Time { return w.base.Add(time.Duration(n * float64(24*time.Hour))) }

func (w *world) stats(f *repository.HubFaction) *Stats {
	w.t.Helper()
	st, err := w.svc.GetFactionStats(w.ctx, f.OrganizationID, f.InstallationID, f.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	return st
}

// fresh recomputes without the cache.
func (w *world) fresh(f *repository.HubFaction) *Stats {
	w.t.Helper()
	w.svc.Invalidate(f.OrganizationID, f.InstallationID, f.ID)
	return w.stats(f)
}

func (st *Stats) member(discordUser string) *MemberContribution {
	for i := range st.MemberContributions {
		if st.MemberContributions[i].DiscordUserID == discordUser {
			return &st.MemberContributions[i]
		}
	}
	return nil
}

func (w *world) count(sql string, args ...any) int { return int(w.one(sql, args...)) }

var _ = factionhub.RoleLeader
