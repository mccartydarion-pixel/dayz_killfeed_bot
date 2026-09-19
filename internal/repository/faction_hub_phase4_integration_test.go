//go:build integration

package repository

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/assetstore"
	"github.com/yourname/dayz-killfeed/internal/factionhub"
)

// Real-PostgreSQL tests for Faction Hub Phase 4: logo metadata, leadership transfer, self-leave
// and the PostgreSQL-backed asset store. Fixtures come from newHubWorld (throwaway database only).

func (w *hubWorld) logoInput(factionID int64, tag string) NewLogoAsset {
	id := testUUID()
	return NewLogoAsset{PublicID: id, StorageKey: fmt.Sprintf("factions/%d/%d/%d/%s.png", w.orgOf(factionID), w.instOf(factionID), factionID, id),
		ContentType: "image/png", OriginalFilename: tag + ".png", SizeBytes: 1234, Width: 256, Height: 256}
}

func TestHubLogoAssetMetadataLifecycle(t *testing.T) {
	w := newHubWorld(t)
	u := w.newUsers(5)
	leader, officer, member, outsider := u[0], u[1], u[2], u[3]
	f := w.create(w.inst1, leader, "Logo House", "LH", "OPEN")
	w.join(f, leader, officer)
	w.join(f, leader, member)
	if _, err := w.repo.PromoteMember(w.ctx, w.org1, w.inst1, f.ID, w.memberIDOf(f.ID, officer), leader); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.repo.Get(w.ctx, w.org1, w.inst1, f.ID); got.Logo != nil {
		t.Fatal("a new faction has the default logo (nil)")
	}

	// Only the LEADER may set or delete the logo.
	for who, user := range map[string]int64{"officer": officer, "member": member, "outsider": outsider} {
		if _, _, err := w.repo.ReplaceLogo(w.ctx, w.org1, w.inst1, f.ID, user, w.logoInput(f.ID, who)); !errors.Is(err, factionhub.ErrForbidden) {
			t.Fatalf("%s replacing logo: %v", who, err)
		}
		if _, err := w.repo.DeleteLogo(w.ctx, w.org1, w.inst1, f.ID, user); !errors.Is(err, factionhub.ErrForbidden) {
			t.Fatalf("%s deleting logo: %v", who, err)
		}
		if err := w.repo.RequireLeader(w.ctx, w.org1, w.inst1, f.ID, user); !errors.Is(err, factionhub.ErrForbidden) {
			t.Fatalf("%s RequireLeader: %v", who, err)
		}
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_assets WHERE faction_id=$1`, f.ID); n != 0 {
		t.Fatalf("forbidden attempts must record nothing, got %d asset rows", n)
	}
	if err := w.repo.RequireLeader(w.ctx, w.org1, w.inst1, f.ID, leader); err != nil {
		t.Fatal(err)
	}

	first := w.logoInput(f.ID, "first")
	a1, old, err := w.repo.ReplaceLogo(w.ctx, w.org1, w.inst1, f.ID, leader, first)
	if err != nil || old != nil {
		t.Fatalf("first logo: %+v old=%v err=%v", a1, old, err)
	}
	if a1.PublicID != first.PublicID || a1.StorageKey != first.StorageKey || a1.ContentType != "image/png" || a1.Width != 256 || a1.SizeBytes != 1234 || a1.FactionID != f.ID {
		t.Fatalf("asset metadata: %+v", a1)
	}
	got, _ := w.repo.Get(w.ctx, w.org1, w.inst1, f.ID)
	if got.Logo == nil || got.Logo.ID != a1.ID {
		t.Fatalf("profile must show the logo: %+v", got.Logo)
	}
	dir, _, _ := w.repo.Directory(w.ctx, w.org1, w.inst1, HubDirectoryQuery{Limit: 10})
	if len(dir) != 1 || dir[0].Logo == nil || dir[0].Logo.PublicID != first.PublicID {
		t.Fatalf("directory must show the logo: %+v", dir)
	}
	if my, _ := w.repo.MyFaction(w.ctx, w.org1, w.inst1, leader); my.Faction == nil || my.Faction.Logo == nil {
		t.Fatal("my faction must show the logo")
	}
	if pub, err := w.repo.AssetByPublicID(w.ctx, first.PublicID); err != nil || pub.StorageKey != first.StorageKey {
		t.Fatalf("public lookup: %+v %v", pub, err)
	}
	if _, err := w.repo.AssetByPublicID(w.ctx, testUUID()); !errors.Is(err, factionhub.ErrNotFound) {
		t.Fatalf("unknown public id: %v", err)
	}

	// Replacement: new row, pointer moved, OLD row gone (its URL stops resolving), old asset returned.
	second := w.logoInput(f.ID, "second")
	a2, old, err := w.repo.ReplaceLogo(w.ctx, w.org1, w.inst1, f.ID, leader, second)
	if err != nil || old == nil || old.ID != a1.ID || old.StorageKey != first.StorageKey {
		t.Fatalf("replacement: %+v old=%+v err=%v", a2, old, err)
	}
	if _, err := w.repo.AssetByPublicID(w.ctx, first.PublicID); !errors.Is(err, factionhub.ErrNotFound) {
		t.Fatalf("the replaced logo's public id must stop resolving: %v", err)
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_assets WHERE faction_id=$1`, f.ID); n != 1 {
		t.Fatalf("exactly one asset row after replacement, got %d", n)
	}
	if got, _ := w.repo.Get(w.ctx, w.org1, w.inst1, f.ID); got.Logo == nil || got.Logo.ID != a2.ID {
		t.Fatal("faction must point at the new logo")
	}
	refs, err := w.repo.ReferencedAssetKeys(w.ctx, []string{first.StorageKey, second.StorageKey, "factions/none.png"})
	if err != nil || refs[first.StorageKey] || !refs[second.StorageKey] || refs["factions/none.png"] {
		t.Fatalf("referenced keys: %v %v", refs, err)
	}

	// Delete: back to the default logo; idempotent.
	removed, err := w.repo.DeleteLogo(w.ctx, w.org1, w.inst1, f.ID, leader)
	if err != nil || removed == nil || removed.ID != a2.ID {
		t.Fatalf("delete: %+v %v", removed, err)
	}
	if got, _ := w.repo.Get(w.ctx, w.org1, w.inst1, f.ID); got.Logo != nil {
		t.Fatal("after delete the faction has the default logo")
	}
	if again, err := w.repo.DeleteLogo(w.ctx, w.org1, w.inst1, f.ID, leader); err != nil || again != nil {
		t.Fatalf("deleting again is a no-op: %+v %v", again, err)
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_assets WHERE faction_id=$1`, f.ID); n != 0 {
		t.Fatalf("no asset rows after delete, got %d", n)
	}
}

func TestHubLogoSchemaGuarantees(t *testing.T) {
	w := newHubWorld(t)
	u := w.newUsers(3)
	a := w.create(w.inst1, u[0], "Schema A", "SA", "OPEN")
	b := w.create(w.inst1, u[1], "Schema B", "SB", "OPEN")
	c := w.create(w.inst1b, u[2], "Schema C", "SC", "OPEN")
	assetA, _, err := w.repo.ReplaceLogo(w.ctx, w.org1, w.inst1, a.ID, u[0], w.logoInput(a.ID, "a"))
	if err != nil {
		t.Fatal(err)
	}

	// A faction can only ever point at one of ITS OWN assets.
	if _, err := w.db.Pool.Exec(w.ctx, `UPDATE hub_factions SET logo_asset_id=$1 WHERE id=$2`, assetA.ID, b.ID); err == nil {
		t.Fatal("pointing faction B at faction A's asset must be rejected by the schema")
	}
	if _, err := w.db.Pool.Exec(w.ctx, `UPDATE hub_factions SET logo_asset_id=$1 WHERE id=$2`, assetA.ID, c.ID); err == nil {
		t.Fatal("pointing a faction on another installation at this asset must be rejected")
	}
	insert := func(id, key, ct string, size, width int, fname string, inst, org int64) error {
		_, err := w.db.Pool.Exec(w.ctx, `INSERT INTO hub_faction_assets(public_id, organization_id, installation_id, faction_id, asset_type, storage_key, content_type, size_bytes, width, height, original_filename)
VALUES($1::uuid,$2,$3,$4,'LOGO',$5,$6,$7,$8,200,$9)`, id, org, inst, b.ID, key, ct, size, width, fname)
		return err
	}
	ok := func() (string, string) { id := testUUID(); return id, "factions/x/" + id + ".png" }
	id, key := ok()
	if err := insert(id, key, "image/png", 100, 200, "ok.png", w.inst1, w.org1); err != nil {
		t.Fatalf("a valid asset row must insert: %v", err)
	}
	cases := map[string]func() error{
		"asset in another installation than its faction": func() error { id, k := ok(); return insert(id, k, "image/png", 100, 200, "", w.inst1b, w.org1) },
		"asset claiming another organization":            func() error { id, k := ok(); return insert(id, k, "image/png", 100, 200, "", w.inst1, w.org2) },
		"duplicate storage key":                          func() error { id, _ := ok(); return insert(id, key, "image/png", 100, 200, "", w.inst1, w.org1) },
		"duplicate public id":                            func() error { _, k := ok(); return insert(id, k, "image/png", 100, 200, "", w.inst1, w.org1) },
		"svg content type":                               func() error { id, k := ok(); return insert(id, k, "image/svg+xml", 100, 200, "", w.inst1, w.org1) },
		"gif content type":                               func() error { id, k := ok(); return insert(id, k, "image/gif", 100, 200, "", w.inst1, w.org1) },
		"zero size":                                      func() error { id, k := ok(); return insert(id, k, "image/png", 0, 200, "", w.inst1, w.org1) },
		"oversize":                                       func() error { id, k := ok(); return insert(id, k, "image/png", 6<<20, 200, "", w.inst1, w.org1) },
		"absurd width":                                   func() error { id, k := ok(); return insert(id, k, "image/png", 100, 100000, "", w.inst1, w.org1) },
		"traversal storage key": func() error {
			id, _ := ok()
			return insert(id, "factions/../../etc/passwd", "image/png", 100, 200, "", w.inst1, w.org1)
		},
		"absolute storage key": func() error {
			id, _ := ok()
			return insert(id, "/etc/passwd", "image/png", 100, 200, "", w.inst1, w.org1)
		},
		"storage key with a url": func() error {
			id, _ := ok()
			return insert(id, "https://evil.example/x.png", "image/png", 100, 200, "", w.inst1, w.org1)
		},
		"over-long original filename": func() error {
			id, k := ok()
			return insert(id, k, "image/png", 100, 200, string(make([]byte, 200)), w.inst1, w.org1)
		},
	}
	for name, fn := range cases {
		if err := fn(); err == nil {
			t.Errorf("%s must be rejected by the schema", name)
		}
	}
	// Deleting a faction removes its asset rows (the bytes are swept as orphans).
	if _, err := w.db.Pool.Exec(w.ctx, `DELETE FROM hub_factions WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_assets WHERE faction_id=$1`, a.ID); n != 0 {
		t.Fatalf("asset rows must cascade with the faction, got %d", n)
	}
}

func TestHubPhase4TenantIsolationEveryMethod(t *testing.T) {
	w := newHubWorld(t)
	u := w.newUsers(3)
	f := w.create(w.inst1, u[0], "Phase Four Iso", "P4", "OPEN")
	memberID := w.join(f, u[0], u[1])
	if _, _, err := w.repo.ReplaceLogo(w.ctx, w.org1, w.inst1, f.ID, u[0], w.logoInput(f.ID, "iso")); err != nil {
		t.Fatal(err)
	}
	other := w.create(w.inst1b, u[2], "Other Tenant", "OT", "OPEN")
	otherMember := w.join(other, u[2], w.newUser())

	type call struct {
		name string
		fn   func(org, inst int64) error
	}
	calls := []call{
		{"RequireLeader", func(o, i int64) error { return w.repo.RequireLeader(w.ctx, o, i, f.ID, u[0]) }},
		{"ReplaceLogo", func(o, i int64) error {
			_, _, err := w.repo.ReplaceLogo(w.ctx, o, i, f.ID, u[0], w.logoInput(f.ID, "x"))
			return err
		}},
		{"DeleteLogo", func(o, i int64) error { _, err := w.repo.DeleteLogo(w.ctx, o, i, f.ID, u[0]); return err }},
		{"TransferLeadership", func(o, i int64) error {
			_, _, err := w.repo.TransferLeadership(w.ctx, o, i, f.ID, u[0], memberID)
			return err
		}},
		{"LeaveFaction", func(o, i int64) error { _, err := w.repo.LeaveFaction(w.ctx, o, i, f.ID, u[1]); return err }},
	}
	for _, scope := range []struct {
		label     string
		org, inst int64
	}{{"other org, real installation", w.org2, w.inst1}, {"real org, other installation", w.org1, w.inst1b}, {"both wrong", w.org2, w.inst2}} {
		for _, c := range calls {
			if err := c.fn(scope.org, scope.inst); !errors.Is(err, factionhub.ErrNotFound) {
				t.Errorf("%s via %s: want ErrNotFound, got %v", c.name, scope.label, err)
			}
		}
	}
	// A member id from ANOTHER faction is not a valid transfer target (even for a real leader).
	if _, _, err := w.repo.TransferLeadership(w.ctx, w.org1, w.inst1, f.ID, u[0], otherMember); !errors.Is(err, factionhub.ErrNotFound) {
		t.Fatalf("cross-faction transfer target must be not found, got %v", err)
	}
	// Nothing changed.
	if got, _ := w.repo.Get(w.ctx, w.org1, w.inst1, f.ID); got.Logo == nil || got.MemberCount != 2 || w.roleOf(f.ID, u[0]) != factionhub.RoleLeader {
		t.Fatalf("probes must change nothing: %+v", got)
	}
	// Another tenant's faction id is not a way to reach this faction's asset either: the public
	// lookup is by unguessable id, and the row is owned by exactly one faction.
	if got, _ := w.repo.Get(w.ctx, w.org1, w.inst1b, other.ID); got.Logo != nil {
		t.Fatal("the other tenant's faction has no logo")
	}
}

func TestHubLeadershipTransfer(t *testing.T) {
	w := newHubWorld(t)
	u := w.newUsers(6)
	leader, officer, member, outsider := u[0], u[1], u[2], u[3]
	f := w.create(w.inst1, leader, "Succession", "SUC", "OPEN")
	mOfficer, mMember := w.join(f, leader, officer), w.join(f, leader, member)
	if _, err := w.repo.PromoteMember(w.ctx, w.org1, w.inst1, f.ID, mOfficer, leader); err != nil {
		t.Fatal(err)
	}
	mLeader := w.memberIDOf(f.ID, leader)
	transfer := func(actor, target int64) (HubMember, HubMember, error) {
		return w.repo.TransferLeadership(w.ctx, w.org1, w.inst1, f.ID, actor, target)
	}
	leaders := func() int {
		return w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE faction_id=$1 AND role_key='LEADER'`, f.ID)
	}

	// Only the current LEADER may transfer.
	for who, actor := range map[string]int64{"officer": officer, "member": member, "outsider": outsider} {
		if _, _, err := transfer(actor, mMember); !errors.Is(err, factionhub.ErrForbidden) {
			t.Fatalf("%s transferring: %v", who, err)
		}
	}
	// The leader cannot transfer to themself, and an unknown member id is not found.
	if _, _, err := transfer(leader, mLeader); !errors.Is(err, factionhub.ErrAlreadyLeader) {
		t.Fatalf("transfer to self: %v", err)
	}
	if _, _, err := transfer(leader, 999999999); !errors.Is(err, factionhub.ErrNotFound) {
		t.Fatalf("unknown target: %v", err)
	}
	if leaders() != 1 || w.roleOf(f.ID, leader) != factionhub.RoleLeader {
		t.Fatal("rejected transfers must change nothing")
	}

	// LEADER -> MEMBER.
	next, prev, err := transfer(leader, mMember)
	if err != nil || next.RoleKey != factionhub.RoleLeader || next.User.ID != member || prev.RoleKey != factionhub.RoleOfficer || prev.User.ID != leader {
		t.Fatalf("leader -> member: %+v %+v %v", next, prev, err)
	}
	if leaders() != 1 || w.roleOf(f.ID, member) != factionhub.RoleLeader || w.roleOf(f.ID, leader) != factionhub.RoleOfficer {
		t.Fatalf("roles after transfer: member=%s leader=%s (%d leaders)", w.roleOf(f.ID, member), w.roleOf(f.ID, leader), leaders())
	}
	if got, _ := w.repo.Get(w.ctx, w.org1, w.inst1, f.ID); got.CreatedByUserID != leader {
		t.Fatal("the founding account is history and stays recorded")
	}
	// The former leader (now OFFICER) can no longer transfer, edit or leave-as-leader.
	if _, _, err := transfer(leader, mOfficer); !errors.Is(err, factionhub.ErrForbidden) {
		t.Fatalf("former leader transferring: %v", err)
	}
	desc := "edited by the old leader"
	if _, err := w.repo.UpdateFaction(w.ctx, w.org1, w.inst1, f.ID, leader, HubFactionUpdate{Description: &desc}); !errors.Is(err, factionhub.ErrForbidden) {
		t.Fatalf("former leader editing: %v", err)
	}
	if _, err := w.repo.UpdateFaction(w.ctx, w.org1, w.inst1, f.ID, member, HubFactionUpdate{Description: &desc}); err != nil {
		t.Fatalf("the new leader edits: %v", err)
	}

	// LEADER -> OFFICER (the new leader hands over to the existing officer).
	next, prev, err = transfer(member, mOfficer)
	if err != nil || next.User.ID != officer || next.RoleKey != factionhub.RoleLeader || prev.User.ID != member || prev.RoleKey != factionhub.RoleOfficer {
		t.Fatalf("leader -> officer: %+v %+v %v", next, prev, err)
	}
	if leaders() != 1 {
		t.Fatalf("exactly one leader, got %d", leaders())
	}
	// The old leader is an OFFICER: a former member removed by the new leader can rejoin elsewhere.
	if _, err := w.repo.RemoveMember(w.ctx, w.org1, w.inst1, f.ID, mLeader, officer); err != nil {
		t.Fatalf("the new leader may remove the demoted ex-leader (an OFFICER): %v", err)
	}
}

func TestHubConcurrentLeadershipTransfers(t *testing.T) {
	w := newHubWorld(t)
	leader := w.newUser()
	f := w.create(w.inst1, leader, "Coup Attempt", "COUP", "OPEN")
	for round := 0; round < 10; round++ {
		a, b, c := w.newUser(), w.newUser(), w.newUser()
		mA, mB, mC := w.join(f, leader, a), w.join(f, leader, b), w.join(f, leader, c)
		targets := []int64{mA, mB, mC, mA}
		errs := runConcurrently(len(targets), func(i int) error {
			_, _, err := w.repo.TransferLeadership(w.ctx, w.org1, w.inst1, f.ID, leader, targets[i])
			return err
		})
		if ok := countOK(errs); ok != 1 {
			t.Fatalf("round %d: exactly one simultaneous transfer may win, got %d (%v)", round, ok, errs)
		}
		for _, e := range errs {
			if e != nil && !errors.Is(e, factionhub.ErrForbidden) {
				t.Fatalf("round %d: losers must be ErrForbidden (they are no longer the leader), got %v", round, e)
			}
		}
		if n := w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE faction_id=$1 AND role_key='LEADER'`, f.ID); n != 1 {
			t.Fatalf("round %d: %d leaders", round, n)
		}
		// Hand leadership back to the original account for the next round and clear the extras.
		newLeader := w.one(`SELECT user_id FROM hub_faction_members WHERE faction_id=$1 AND role_key='LEADER'`, f.ID)
		if _, _, err := w.repo.TransferLeadership(w.ctx, w.org1, w.inst1, f.ID, newLeader, w.memberIDOf(f.ID, leader)); err != nil {
			t.Fatalf("round %d: hand back: %v", round, err)
		}
		if _, err := w.db.Pool.Exec(w.ctx, `DELETE FROM hub_faction_members WHERE faction_id=$1 AND role_key<>'LEADER'`, f.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHubTransferRacingLeaveAndRemoval(t *testing.T) {
	w := newHubWorld(t)
	leader := w.newUser()
	f := w.create(w.inst1, leader, "Handover Race", "HR", "OPEN")
	for round := 0; round < 12; round++ {
		target := w.newUser()
		mTarget := w.join(f, leader, target)
		// The target leaves while the leader transfers to it, or the leader removes it meanwhile.
		errs := runConcurrently(3, func(i int) error {
			switch i {
			case 0:
				_, _, err := w.repo.TransferLeadership(w.ctx, w.org1, w.inst1, f.ID, leader, mTarget)
				return err
			case 1:
				_, err := w.repo.LeaveFaction(w.ctx, w.org1, w.inst1, f.ID, target)
				return err
			default:
				_, err := w.repo.RemoveMember(w.ctx, w.org1, w.inst1, f.ID, mTarget, leader)
				return err
			}
		})
		for _, e := range errs {
			if e != nil && !errors.Is(e, factionhub.ErrForbidden) && !errors.Is(e, factionhub.ErrNotFound) &&
				!errors.Is(e, factionhub.ErrLeadershipTransferRequired) && !errors.Is(e, factionhub.ErrLeaderProtected) && !errors.Is(e, factionhub.ErrAlreadyLeader) {
				t.Fatalf("round %d: unexpected error %v", round, e)
			}
		}
		if n := w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE faction_id=$1 AND role_key='LEADER'`, f.ID); n != 1 {
			t.Fatalf("round %d: %d leaders", round, n)
		}
		// Reset: whoever leads now hands leadership back to the founder if needed, then drop everyone else.
		cur := w.one(`SELECT user_id FROM hub_faction_members WHERE faction_id=$1 AND role_key='LEADER'`, f.ID)
		if cur != leader {
			if _, _, err := w.repo.TransferLeadership(w.ctx, w.org1, w.inst1, f.ID, cur, w.memberIDOf(f.ID, leader)); err != nil {
				t.Fatalf("round %d: hand back: %v", round, err)
			}
		}
		_, _ = w.db.Pool.Exec(w.ctx, `DELETE FROM hub_faction_members WHERE faction_id=$1 AND user_id<>$2`, f.ID, leader)
		_, _ = w.db.Pool.Exec(w.ctx, `UPDATE hub_faction_members SET role_key='LEADER' WHERE faction_id=$1 AND user_id=$2 AND role_key<>'LEADER'`, f.ID, leader)
	}
}

func TestHubSelfLeave(t *testing.T) {
	w := newHubWorld(t)
	u := w.newUsers(6)
	leader, officer, member, outsider := u[0], u[1], u[2], u[3]
	f := w.create(w.inst1, leader, "Open Door", "OD", "OPEN")
	other := w.create(w.inst1, u[4], "Another Home", "AH", "OPEN")
	mOfficer := w.join(f, leader, officer)
	w.join(f, leader, member)
	if _, err := w.repo.PromoteMember(w.ctx, w.org1, w.inst1, f.ID, mOfficer, leader); err != nil {
		t.Fatal(err)
	}
	// Give the future ex-member some application history in this faction.
	var acceptedApp int64
	if err := w.db.Pool.QueryRow(w.ctx, `SELECT id FROM hub_faction_applications WHERE faction_id=$1 AND user_id=$2 AND status='ACCEPTED'`, f.ID, member).Scan(&acceptedApp); err != nil {
		t.Fatal(err)
	}
	leave := func(user int64) (*HubMember, error) { return w.repo.LeaveFaction(w.ctx, w.org1, w.inst1, f.ID, user) }

	// The LEADER cannot leave while leading.
	if _, err := leave(leader); !errors.Is(err, factionhub.ErrLeadershipTransferRequired) {
		t.Fatalf("leader leaving: %v", err)
	}
	if w.roleOf(f.ID, leader) != factionhub.RoleLeader {
		t.Fatal("a refused leave changes nothing")
	}
	// A non-member is refused; a member of a DIFFERENT faction on the installation is too.
	if _, err := leave(outsider); !errors.Is(err, factionhub.ErrForbidden) {
		t.Fatalf("outsider leaving: %v", err)
	}
	if _, err := leave(u[4]); !errors.Is(err, factionhub.ErrForbidden) {
		t.Fatalf("a member of another faction leaving this one: %v", err)
	}
	if w.roleOf(other.ID, u[4]) != factionhub.RoleLeader {
		t.Fatal("the other faction is untouched")
	}

	// MEMBER leaves; the history row stays ACCEPTED and does not come back to life.
	left, err := leave(member)
	if err != nil || left.RoleKey != factionhub.RoleMember || left.User.ID != member {
		t.Fatalf("member leaving: %+v %v", left, err)
	}
	var status string
	if err := w.db.Pool.QueryRow(w.ctx, `SELECT status FROM hub_faction_applications WHERE id=$1`, acceptedApp).Scan(&status); err != nil || status != "ACCEPTED" {
		t.Fatalf("application history must stay as it was (ACCEPTED), got %q %v", status, err)
	}
	if pending := w.count(`SELECT COUNT(*) FROM hub_faction_applications WHERE user_id=$1 AND status='PENDING'`, member); pending != 0 {
		t.Fatalf("leaving creates no pending application, got %d", pending)
	}
	if _, err := leave(member); !errors.Is(err, factionhub.ErrForbidden) {
		t.Fatalf("a second leave is a safe 403 (not a member): %v", err)
	}
	if got, _ := w.repo.Get(w.ctx, w.org1, w.inst1, f.ID); got.MemberCount != 2 {
		t.Fatalf("member count after leave: %d", got.MemberCount)
	}

	// The former member is immediately eligible: apply elsewhere, apply here again, or found a faction.
	if _, err := w.repo.Apply(w.ctx, w.org1, w.inst1, other.ID, member, "moving on"); err != nil {
		t.Fatalf("former member applying elsewhere: %v", err)
	}
	if _, err := w.repo.Apply(w.ctx, w.org1, w.inst1, f.ID, member, "back again"); err != nil {
		t.Fatalf("former member re-applying: %v", err)
	}
	// OFFICER leaves too, then founds a faction of their own.
	if _, err := leave(officer); err != nil {
		t.Fatalf("officer leaving: %v", err)
	}
	if g := w.create(w.inst1, officer, "Ex Officer Corps", "XO", "OPEN"); g == nil {
		t.Fatal("a former officer can found a faction")
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE faction_id=$1 AND role_key='LEADER'`, f.ID); n != 1 {
		t.Fatalf("still exactly one leader, got %d", n)
	}
	// After a real handover the old leader (now OFFICER) can leave.
	stay := w.newUser()
	mStay := w.join(f, leader, stay)
	if _, _, err := w.repo.TransferLeadership(w.ctx, w.org1, w.inst1, f.ID, leader, mStay); err != nil {
		t.Fatal(err)
	}
	if _, err := leave(leader); err != nil {
		t.Fatalf("the former leader may leave after transferring: %v", err)
	}
}

func TestHubConcurrentLeavesAndJoins(t *testing.T) {
	w := newHubWorld(t)
	leader := w.newUser()
	f := w.create(w.inst1, leader, "Revolving Door", "RD", "OPEN")
	for round := 0; round < 10; round++ {
		user := w.newUser()
		w.join(f, leader, user)
		errs := runConcurrently(4, func(i int) error {
			_, err := w.repo.LeaveFaction(w.ctx, w.org1, w.inst1, f.ID, user)
			return err
		})
		if ok := countOK(errs); ok != 1 {
			t.Fatalf("round %d: exactly one simultaneous leave may succeed, got %d (%v)", round, ok, errs)
		}
		for _, e := range errs {
			if e != nil && !errors.Is(e, factionhub.ErrForbidden) {
				t.Fatalf("round %d: repeated leaves must be a safe ErrForbidden, got %v", round, e)
			}
		}
		if n := w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE installation_id=$1 AND user_id=$2`, w.inst1, user); n != 0 {
			t.Fatalf("round %d: membership must be gone, got %d", round, n)
		}
	}
}

func TestPostgresAssetStoreContract(t *testing.T) {
	w := newHubWorld(t)
	s := NewPostgresAssetStore(w.db.Pool)
	ctx := context.Background()
	key := fmt.Sprintf("factions/test/%d/%s.png", w.suffix, testUUID())
	t.Cleanup(func() { _ = s.Delete(ctx, key) })

	if err := s.Put(ctx, "../evil.png", "image/png", []byte("x")); !errors.Is(err, assetstore.ErrInvalidKey) {
		t.Fatalf("traversal key: %v", err)
	}
	if _, _, err := s.Get(ctx, "/etc/passwd"); !errors.Is(err, assetstore.ErrInvalidKey) {
		t.Fatalf("absolute key on get: %v", err)
	}
	payload := []byte("\x89PNG\r\n\x1a\nnot really a png but opaque bytes \x00\x01\x02")
	if err := s.Put(ctx, key, "image/png", payload); err != nil {
		t.Fatal(err)
	}
	got, ct, err := s.Get(ctx, key)
	if err != nil || string(got) != string(payload) || ct != "image/png" {
		t.Fatalf("round trip: %q %q %v", got, ct, err)
	}
	if err := s.Put(ctx, key, "image/jpeg", []byte("v2")); err != nil { // Put replaces
		t.Fatal(err)
	}
	if got, ct, _ := s.Get(ctx, key); string(got) != "v2" || ct != "image/jpeg" {
		t.Fatalf("replace: %q %q", got, ct)
	}
	keys, err := s.List(ctx, fmt.Sprintf("factions/test/%d/", w.suffix), time.Now().Add(time.Minute), 10)
	if err != nil || len(keys) != 1 || keys[0] != key {
		t.Fatalf("list: %v %v", keys, err)
	}
	if keys, _ := s.List(ctx, fmt.Sprintf("factions/test/%d/", w.suffix), time.Now().Add(-time.Hour), 10); len(keys) != 0 {
		t.Fatalf("a fresh object must not list as old: %v", keys)
	}
	if keys, _ := s.List(ctx, "factions/te%t/", time.Now().Add(time.Minute), 10); len(keys) != 0 {
		t.Fatal("LIKE wildcards in the prefix are literal")
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(ctx, key); !errors.Is(err, assetstore.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("delete is idempotent: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ { // concurrent Puts of one key converge
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.Put(ctx, key, "image/png", []byte(fmt.Sprint(i)))
		}(i)
	}
	wg.Wait()
	if _, _, err := s.Get(ctx, key); err != nil {
		t.Fatalf("concurrent puts: %v", err)
	}
}

// testUUID returns a random version-4 UUID string.
func testUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0F | 0x40
	b[8] = b[8]&0x3F | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
