//go:build integration

package app

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/vip"
)

type fakeRoles struct {
	mu      sync.Mutex
	added   []string
	removed []string
	fail    bool
}

func (f *fakeRoles) GuildMemberRoleAdd(_, user, role string, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("50013 Missing Permissions")
	}
	f.added = append(f.added, user+":"+role)
	return nil
}

func (f *fakeRoles) GuildMemberRoleRemove(_, user, role string, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, user+":"+role)
	return nil
}

func TestVIPTiers(t *testing.T) {
	w := newStandoutWorld(t)
	ctx := context.Background()
	pool := w.a.DB.Pool
	w.a.VIP = repository.NewVIPRepository(pool)
	w.a.Rewards = repository.NewRewardRepository(pool)
	roles := &fakeRoles{}
	w.a.VIPRoles = roles
	owner := w.f.OwnerDiscordID

	saveTier := func(body map[string]any) *vip.Tier {
		rr := w.call(w.a.handleSaveVIPTier, http.MethodPut, w.path("/vip/tiers"), owner, body, nil)
		if rr.Code != http.StatusOK {
			t.Logf("save tier: %d %s", rr.Code, rr.Body.String())
			return nil
		}
		tier := decodeBody[vip.Tier](t, rr)
		return &tier
	}
	if saveTier(map[string]any{"name": "Bad", "badge": "x", "color": "red"}) != nil {
		t.Fatal("invalid colour accepted")
	}
	gold := saveTier(map[string]any{"name": "Gold", "badge": "💎 VIP", "color": "#E7B94A", "discordRoleId": "123456789012345678", "rewardMultiplier": 1.5})
	if gold == nil || gold.ID == 0 {
		t.Fatal("tier not saved")
	}
	if saveTier(map[string]any{"name": "Gold", "badge": "x"}) != nil {
		t.Fatal("duplicate tier name accepted")
	}

	linked, unlinked := w.player("Linked"), w.player("Unlinked")
	if _, err := pool.Exec(ctx, `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,verified_at) VALUES($1,$2,'555000111222333444','VERIFIED',NOW())`, w.guildID, linked); err != nil {
		t.Fatal(err)
	}
	grant := func(player int64, days int) (*repository.VIPMember, int) {
		rr := w.call(w.a.handleGrantVIP, http.MethodPost, w.path("/vip/members"), owner, map[string]any{"playerId": player, "tierId": gold.ID, "days": days}, nil)
		if rr.Code != http.StatusOK {
			return nil, rr.Code
		}
		m := decodeBody[repository.VIPMember](t, rr)
		return &m, rr.Code
	}
	m1, _ := grant(linked, 0)
	if m1 == nil || !m1.Active || m1.RoleError != "" || len(roles.added) != 1 || roles.added[0] != "555000111222333444:123456789012345678" {
		t.Fatalf("grant with role: %+v %v", m1, roles.added)
	}
	if _, code := grant(linked, 0); code != http.StatusBadRequest {
		t.Fatalf("double grant: %d", code)
	}
	m2, _ := grant(unlinked, 30)
	if m2 == nil || m2.RoleError == "" || m2.ExpiresAt == nil {
		t.Fatalf("unlinked member must say why no role was given: %+v", m2)
	}
	if rr := w.call(w.a.handleGrantVIP, http.MethodPost, w.path("/vip/members"), "stranger", map[string]any{"playerId": linked, "tierId": gold.ID}, nil); rr.Code == http.StatusOK {
		t.Fatal("non-staff granted VIP")
	}

	// Killfeed badge and reward multiplier.
	if badge, err := w.a.VIP.ActiveBadge(ctx, w.guildID, linked); err != nil || badge != "💎 VIP" {
		t.Fatalf("badge: %q %v", badge, err)
	}
	grants := w.a.applyRewardMultipliers(ctx, w.guildID, []repository.RewardGrant{{PlayerID: linked, Amount: 100}, {PlayerID: w.player("Plain"), Amount: 100}})
	if grants[0].Amount != 150 || grants[1].Amount != 100 {
		t.Fatalf("multipliers: %+v", grants)
	}

	// Deleting a tier with active members is refused.
	if rr := w.call(w.a.handleDeleteVIPTier, http.MethodDelete, w.path("/vip/tiers/x"), owner, nil, map[string]string{"tierID": strconv.FormatInt(gold.ID, 10)}); rr.Code != http.StatusBadRequest {
		t.Fatalf("delete in-use tier: %d", rr.Code)
	}
	// Revoke removes the role; expiry ends the other membership.
	if rr := w.call(w.a.handleRevokeVIP, http.MethodPost, w.path("/vip/members/x/revoke"), owner, nil, map[string]string{"memberID": strconv.FormatInt(m1.ID, 10)}); rr.Code != http.StatusOK || len(roles.removed) != 1 {
		t.Fatalf("revoke: %d %v", rr.Code, roles.removed)
	}
	w.a.runVIPExpiry(ctx, w.guildID, time.Now().Add(31*24*time.Hour))
	if badge, _ := w.a.VIP.ActiveBadge(ctx, w.guildID, unlinked); badge != "" {
		t.Fatal("expired membership still shows a badge")
	}
	if rr := w.call(w.a.handleDeleteVIPTier, http.MethodDelete, w.path("/vip/tiers/x"), owner, nil, map[string]string{"tierID": strconv.FormatInt(gold.ID, 10)}); rr.Code != http.StatusOK {
		t.Fatalf("delete unused tier: %d %s", rr.Code, rr.Body.String())
	}
}
