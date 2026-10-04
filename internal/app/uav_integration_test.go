//go:build integration

package app

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The live map UAV over a real PostgreSQL (docs/LIVE_MAP.md "UAV").

func TestUAVBasicAndPrecision(t *testing.T) {
	lw := newLiveMapWorld(t)
	ctx := context.Background()
	pool := lw.a.DB.Pool
	lw.a.UAV = repository.NewUAVRepository(pool)
	lw.a.EconomyService = economy.NewService(repository.NewEconomyRepository(pool), nil)
	path := map[string]string{"installationID": strconv.FormatInt(lw.f.InstallationID, 10)}
	get := func(actor string) uavViewDTO {
		t.Helper()
		rr := lw.call(lw.a.handlePlayerUAV, http.MethodGet, "/api/saas/player/servers/x/map/uav", actor, nil, path)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET uav: %d %s", rr.Code, rr.Body.String())
		}
		return decodeBody[uavViewDTO](t, rr)
	}
	buy := func(actor, tier string, blocks int, key string) (int, map[string]any) {
		t.Helper()
		rr := lw.call(lw.a.handleBuyUAV, http.MethodPost, "/api/saas/player/servers/x/map/uav", actor, buyUAVBody{Tier: tier, Blocks: blocks, IdempotencyKey: key}, path)
		out := map[string]any{}
		if rr.Code == http.StatusOK {
			out = decodeBody[map[string]any](t, rr)
		}
		return rr.Code, out
	}

	// Off by default: nothing to see, nothing to buy.
	if v := get(lw.deeloID); v.Offer.Enabled || v.Pass != nil || len(v.Players) != 0 {
		t.Fatalf("off by default: %+v", v)
	}
	if code, _ := buy(lw.deeloID, "BASIC", 1, "uav-key-0001"); code != http.StatusConflict {
		t.Fatalf("closed: %d", code)
	}
	settings := repository.UAVSettings{Enabled: true, BlockMinutes: 15, BasicPrice: 100, PrecisionEnabled: true, PrecisionPrice: 300, GhostPrice: 200, MaxBlocks: 4, ShareFaction: true}
	if code, body := lw.put(lw.a.handleSaveUAV, "/map/uav", lw.f.OwnerDiscordID, repository.UAVSettings{Enabled: true, BlockMinutes: 1}); code != http.StatusBadRequest {
		t.Fatalf("a 1-minute block is refused: %d %v", code, body)
	}
	if code, body := lw.put(lw.a.handleSaveUAV, "/map/uav", lw.f.OwnerDiscordID, settings); code != http.StatusOK {
		t.Fatalf("save: %d %v", code, body)
	}
	if code, _ := lw.put(lw.a.handleSaveUAV, "/map/uav", lw.mateID, settings); code != http.StatusForbidden {
		t.Fatalf("a player cannot change the UAV: %d", code)
	}

	// Paying: not enough points, then a basic UAV for 30 minutes; a replay charges nothing.
	if code, _ := buy(lw.deeloID, "BASIC", 2, "uav-key-0002"); code == http.StatusOK {
		t.Fatal("no points, no UAV")
	}
	if _, err := lw.a.EconomyService.Credit(ctx, economy.Request{GuildID: lw.guildID, PlayerID: lw.deelo, Amount: 1000, Type: economy.TypeSystemReward, ReferenceID: "uav-test"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := buy(lw.deeloID, "BASIC", 5, "uav-key-0003"); code != http.StatusBadRequest {
		t.Fatalf("more than 4 blocks is refused: %d", code)
	}
	code, out := buy(lw.deeloID, "BASIC", 2, "uav-key-0004")
	if code != http.StatusOK || out["balance"].(float64) != 800 {
		t.Fatalf("buy basic: %d %v", code, out)
	}
	ends, _ := time.Parse(time.RFC3339, out["pass"].(map[string]any)["endsAt"].(string))
	if d := time.Until(ends); d < 29*time.Minute || d > 31*time.Minute {
		t.Fatalf("two 15-minute blocks: %v", d)
	}
	if code, again := buy(lw.deeloID, "BASIC", 2, "uav-key-0004"); code != http.StatusOK || again["balance"].(float64) != 800 {
		t.Fatalf("a replay charges nothing: %d %v", code, again)
	}

	// Basic: a dot per connected player (Deelo and Stranger), no names.
	v := get(lw.deeloID)
	if v.Pass == nil || v.Pass.Tier != repository.UAVBasic || !v.Pass.Mine || len(v.Players) != 2 {
		t.Fatalf("basic view: %+v", v)
	}
	for _, p := range v.Players {
		if p.Name != "" || p.FactionTag != "" || p.LastWeapon != "" || p.AliveSeconds != nil {
			t.Fatalf("a basic UAV shows only positions: %+v", p)
		}
	}
	// Mate is in Deelo's faction, so the UAV is shared with them.
	if mv := get(lw.mateID); mv.Pass == nil || mv.Pass.Mine || mv.Pass.BuyerName != "Deelo" {
		t.Fatalf("shared with the faction: %+v", mv.Pass)
	}

	// Precision wins over basic and adds who they are and their last kill weapon this life.
	lw.killAt(lw.deelo, lw.ghost, time.Now().UTC().Add(-5*time.Minute), 4600, 10300)
	if code, out := buy(lw.deeloID, "precision", 1, "uav-key-0005"); code != http.StatusOK || out["balance"].(float64) != 500 {
		t.Fatalf("buy precision: %d %v", code, out)
	}
	v = get(lw.deeloID)
	if v.Pass == nil || v.Pass.Tier != repository.UAVPrecision {
		t.Fatalf("precision view: %+v", v.Pass)
	}
	var stranger, self *uavPlayerDTO
	for i := range v.Players {
		switch v.Players[i].Name {
		case "Stranger":
			stranger = &v.Players[i]
		case "Deelo":
			self = &v.Players[i]
		}
	}
	// Stranger was killed 30 seconds ago: a new life, no kills in it yet.
	if stranger == nil || stranger.LastWeapon != "" || stranger.LifeKills == nil || *stranger.LifeKills != 0 {
		t.Fatalf("stranger under a precision UAV: %+v", stranger)
	}
	// Deelo has not died since killing Ghost with a KA-M five minutes ago.
	if self == nil || !self.Self || self.FactionTag != "RD" || self.LastWeapon != "KA-M" || self.LifeKills == nil || *self.LifeKills != 1 || self.WeaponAgeSec == nil {
		t.Fatalf("the viewer: %+v", self)
	}

	// Ghost: off until the owner sells it; then Deelo vanishes from everyone else's UAV (Mate's shared
	// precision view) but still sees their own dot.
	if code, _ := buy(lw.deeloID, "GHOST", 1, "uav-key-0010"); code != http.StatusConflict {
		t.Fatalf("ghost is off by default: %d", code)
	}
	settings.GhostEnabled = true
	if code, _ := lw.put(lw.a.handleSaveUAV, "/map/uav", lw.f.OwnerDiscordID, settings); code != http.StatusOK {
		t.Fatal("save ghost")
	}
	seesDeelo := func(v uavViewDTO) bool {
		for _, p := range v.Players {
			if p.Name == "Deelo" {
				return true
			}
		}
		return false
	}
	if !seesDeelo(get(lw.mateID)) {
		t.Fatal("before the ghost, Mate's UAV shows Deelo")
	}
	if code, out := buy(lw.deeloID, "GHOST", 1, "uav-key-0011"); code != http.StatusOK || out["balance"].(float64) != 300 {
		t.Fatalf("buy ghost: %d %v", code, out)
	}
	if seesDeelo(get(lw.mateID)) {
		t.Fatal("a ghost is hidden from other players' UAVs")
	}
	if dv := get(lw.deeloID); dv.Ghost == nil || dv.Pass == nil || dv.Pass.Tier != repository.UAVPrecision || !seesDeelo(dv) {
		t.Fatalf("the ghost's own view: ghost %+v pass %+v", dv.Ghost, dv.Pass)
	}

	// Owner's view and switching precision off.
	admin := decodeBody[map[string]any](t, lw.call(lw.a.handleAdminUAV, http.MethodGet, lw.path("/map/uav"), lw.f.OwnerDiscordID, nil, nil))
	sales := admin["sales"].(map[string]any)
	if sales["passes"].(float64) != 3 || sales["points"].(float64) != 700 {
		t.Fatalf("sales: %v", sales)
	}
	settings.PrecisionEnabled = false
	if code, _ := lw.put(lw.a.handleSaveUAV, "/map/uav", lw.f.OwnerDiscordID, settings); code != http.StatusOK {
		t.Fatal("save")
	}
	if code, _ := buy(lw.deeloID, "PRECISION", 1, "uav-key-0006"); code != http.StatusConflict {
		t.Fatalf("precision off: %d", code)
	}
}
