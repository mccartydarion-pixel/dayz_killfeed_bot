//go:build integration

package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/vip"
)

// fakeDMs records direct messages; a user id in closed refuses them, as Discord does for a member
// who does not accept messages from the server.
type fakeDMs struct {
	mu     sync.Mutex
	closed map[string]bool
	sent   map[string][]*discordgo.MessageSend // by user id
}

func (f *fakeDMs) UserChannelCreate(userID string, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	if f.closed[userID] {
		return nil, errors.New("50007 Cannot send messages to this user")
	}
	return &discordgo.Channel{ID: "dm-" + userID}, nil
}

func (f *fakeDMs) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user := strings.TrimPrefix(channelID, "dm-")
	f.sent[user] = append(f.sent[user], data)
	return &discordgo.Message{ID: "m"}, nil
}

func TestVIPGrantTellsThePlayerAndShowsInThePlayerHub(t *testing.T) {
	w := newStandoutWorld(t)
	ctx := context.Background()
	pool := w.a.DB.Pool
	econ := repository.NewEconomyRepository(pool)
	w.a.EconomyService = economy.NewService(econ, nil)
	w.a.EconomyAccounts = economy.NewAccounts(w.a.EconomyService, econ)
	w.a.VIP = repository.NewVIPRepository(pool)
	w.a.VIPRoles = &fakeRoles{}
	dms := &fakeDMs{closed: map[string]bool{}, sent: map[string][]*discordgo.MessageSend{}}
	w.a.VIPNotices = dms
	owner := w.f.OwnerDiscordID

	tier, err := w.a.VIP.SaveTier(ctx, w.guildID, vip.Tier{Name: "Golden Supporter", Badge: "💎 VIP", Color: "#AA00FF", DiscordRoleID: "123456789012345678", RewardMultiplier: 3})
	if err != nil {
		t.Fatal(err)
	}
	link := func(name string) (discordID string, playerID int64) {
		user := syncUser(t, w.a, fmt.Sprintf("vip-%s-%d", strings.ToLower(name), time.Now().UnixNano()), name)
		playerID = w.player(name)
		if _, err := pool.Exec(ctx, `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,verified_at) VALUES($1,$2,$3,'VERIFIED',NOW())`, w.guildID, playerID, user.DiscordUserID); err != nil {
			t.Fatal(err)
		}
		return user.DiscordUserID, playerID
	}
	grant := func(player int64, days int) repository.VIPMember {
		rr := w.call(w.a.handleGrantVIP, http.MethodPost, w.path("/vip/members"), owner, map[string]any{"playerId": player, "tierId": tier.ID, "days": days}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("grant: %d %s", rr.Code, rr.Body.String())
		}
		return decodeBody[repository.VIPMember](t, rr)
	}
	mine := func(discordID string) map[string]any {
		path := fmt.Sprintf("/api/saas/organizations/%d/installations/%d/vip/me", w.f.OrgID, w.f.InstallationID)
		rr := w.call(w.a.handleMyVIP, http.MethodGet, path, discordID, nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("my tier: %d %s", rr.Code, rr.Body.String())
		}
		return decodeBody[map[string]any](t, rr)
	}

	fan, fanID := link("Ceiyxe")
	if got := mine(fan); got["linked"] != true || got["tier"] != nil {
		t.Fatalf("before the grant the player holds nothing: %v", got)
	}

	// The player is told by direct message, with what the tier gives and when it ends.
	m := grant(fanID, 30)
	if m.NoticeError != "" || len(dms.sent[fan]) != 1 {
		t.Fatalf("the player should get one direct message: %q, %d sent", m.NoticeError, len(dms.sent[fan]))
	}
	embed := dms.sent[fan][0].Embeds[0]
	if !strings.Contains(embed.Title, "Golden Supporter") || embed.Color != 0xAA00FF {
		t.Fatalf("unexpected notice: %q colour %x", embed.Title, embed.Color)
	}
	text := embed.Description
	for _, f := range embed.Fields {
		text += "\n" + f.Name + ": " + f.Value
	}
	for _, want := range []string{"💎 VIP", "Discord role", "×3", "<t:", "/dashboard/player"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the notice should mention %q:\n%s", want, text)
		}
	}
	if am := dms.sent[fan][0].AllowedMentions; am == nil || len(am.Parse) != 0 {
		t.Fatal("the notice must not ping anyone")
	}

	// The Player Hub shows the tier.
	held, _ := mine(fan)["tier"].(map[string]any)
	if held == nil || held["name"] != "Golden Supporter" || held["badge"] != "💎 VIP" || held["rewardMultiplier"].(float64) != 3 || held["expiresAt"] == nil || held["discordRole"] != true {
		t.Fatalf("the Player Hub should show the tier: %v", held)
	}

	// Closed direct messages and a missing Discord link are reported to staff, and the tier stands.
	shy, shyID := link("Shy")
	dms.closed[shy] = true
	if m := grant(shyID, 0); !strings.Contains(m.NoticeError, "does not accept direct messages") || !m.Active {
		t.Fatalf("closed DMs should be reported and the tier kept: %+v", m)
	}
	if held, _ := mine(shy)["tier"].(map[string]any); held == nil || held["expiresAt"] != nil {
		t.Fatalf("a tier with no end shows no end: %v", held)
	}
	if m := grant(w.player("Unlinked"), 0); !strings.Contains(m.NoticeError, "no verified Discord link") || !m.Active {
		t.Fatalf("an unlinked player should be reported and the tier kept: %+v", m)
	}

	// Someone with no linked character sees nothing, not an error.
	stranger := syncUser(t, w.a, fmt.Sprintf("vip-stranger-%d", time.Now().UnixNano()), "Stranger")
	if got := mine(stranger.DiscordUserID); got["linked"] != false || got["tier"] != nil {
		t.Fatalf("an unlinked visitor holds nothing: %v", got)
	}

	// Once the tier is ended, the Player Hub stops showing it.
	if rr := w.call(w.a.handleRevokeVIP, http.MethodPost, w.path("/vip/members/x/revoke"), owner, nil, map[string]string{"memberID": fmt.Sprint(m.ID)}); rr.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rr.Code, rr.Body.String())
	}
	if got := mine(fan); got["tier"] != nil {
		t.Fatalf("an ended tier is no longer shown: %v", got)
	}
}
