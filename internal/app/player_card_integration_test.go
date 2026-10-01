//go:build integration

package app

import (
	"bytes"
	"fmt"
	"image/png"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/playercard"
)

// Champion Card over the real routes and a real PostgreSQL (docs/CHAMPION_CARD.md).

func (w *factionWorld) cardPath(installationID int64, suffix string) string {
	return fmt.Sprintf("/api/saas/player/servers/%d/card%s", installationID, suffix)
}

func TestPlayerCardDataImageAndShareLifecycle(t *testing.T) {
	w := newFactionWorld(t)
	actor, other := w.players[0], w.players[1]
	hero := w.linkPlayer(w.a1, actor, "CardHero")
	rival := w.linkPlayer(w.a1, other, "CardRival")
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		w.insertKill(w.a1, hero, now.Add(-time.Duration(i)*time.Minute), i == 0)
	}
	for i := 0; i < 5; i++ {
		w.insertKill(w.a1, rival, now.Add(-time.Duration(i)*time.Minute), false)
	}
	w.insertDeath(w.a1, hero, now)
	w.session(w.a1, hero, now.Add(-2*time.Hour), now.Add(-time.Hour))

	card := w.getJSON(w.cardPath(w.a1.InstallationID, ""), actor)
	if card["playerName"] != "CardHero" || card["kills"].(float64) != 3 || card["deaths"].(float64) != 1 || card["kd"].(float64) != 3 ||
		card["headshots"].(float64) != 1 || card["longestKillMeters"].(float64) != 80 || card["playtimeSeconds"].(float64) != 3600 {
		t.Fatalf("card = %v", card)
	}
	// Rival has five kills, Hero three: rank 2 of 2 ranked players.
	if card["rank"].(float64) != 2 || card["rankedPlayers"].(float64) != 2 {
		t.Fatalf("rank = %v of %v", card["rank"], card["rankedPlayers"])
	}
	if card["share"] != nil || card["factionName"] != nil {
		t.Fatalf("unshared, factionless card carries share/faction: %v", card)
	}
	tiles := card["tiles"].([]any)
	if len(tiles) != 8 || tiles[0].(map[string]any)["value"] != "3" || tiles[7].(map[string]any)["value"] != "#2" {
		t.Fatalf("tiles = %v", tiles)
	}

	img := w.expect(w.do(http.MethodGet, w.cardPath(w.a1.InstallationID, ".png"), actor, nil), http.StatusOK, "own card png")
	if img.ContentType != "image/png" {
		t.Fatalf("content type %q", img.ContentType)
	}
	decoded, err := png.Decode(bytes.NewReader(img.Body))
	if err != nil || decoded.Bounds().Dx() != playercard.Width || decoded.Bounds().Dy() != playercard.Height {
		t.Fatalf("own card is not a %dx%d PNG: %v", playercard.Width, playercard.Height, err)
	}

	// Share: idempotent, public, revocable.
	share := w.expect(w.do(http.MethodPost, w.cardPath(w.a1.InstallationID, "/share"), actor, nil), http.StatusOK, "share").JSON(t)
	token := share["token"].(string)
	if len(token) < 20 || share["imageUrl"] != "https://champion.example/cards/"+token+".png" {
		t.Fatalf("share = %v", share)
	}
	again := w.expect(w.do(http.MethodPost, w.cardPath(w.a1.InstallationID, "/share"), actor, nil), http.StatusOK, "share again").JSON(t)
	if again["token"] != token {
		t.Fatalf("sharing twice produced a second link: %v vs %v", again["token"], token)
	}
	if got := w.getJSON(w.cardPath(w.a1.InstallationID, ""), actor)["share"].(map[string]any)["token"]; got != token {
		t.Fatalf("card does not report its active share: %v", got)
	}

	public := w.publicGet(http.MethodGet, "/cards/"+token+".png", nil)
	if public.Status != http.StatusOK || public.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(public.Header.Get("Cache-Control"), "public, max-age=") {
		t.Fatalf("public card: %d %v", public.Status, public.Header)
	}
	if _, err := png.Decode(bytes.NewReader(public.Body)); err != nil {
		t.Fatalf("public card is not a PNG: %v", err)
	}
	shared := w.expect(w.do(http.MethodGet, "/api/saas/cards/"+token, "", nil), http.StatusOK, "shared card data").JSON(t)
	if shared["playerName"] != "CardHero" || shared["share"].(map[string]any)["token"] != token {
		t.Fatalf("shared card = %v", shared)
	}

	for _, bad := range []string{"/cards/nope.png", "/cards/" + token, "/cards/" + token + ".jpg", "/cards/.png"} {
		if r := w.publicGet(http.MethodGet, bad, nil); r.Status != http.StatusNotFound {
			t.Fatalf("%s: %d", bad, r.Status)
		}
	}

	revoked := w.expect(w.do(http.MethodDelete, w.cardPath(w.a1.InstallationID, "/share"), actor, nil), http.StatusOK, "unshare").JSON(t)
	if revoked["revoked"] != true {
		t.Fatalf("revoke = %v", revoked)
	}
	// Revocation is immediate, even though the image was cached a moment ago.
	if r := w.publicGet(http.MethodGet, "/cards/"+token+".png", nil); r.Status != http.StatusNotFound {
		t.Fatalf("revoked card still served: %d", r.Status)
	}
	if r := w.do(http.MethodGet, "/api/saas/cards/"+token, "", nil); r.Status != http.StatusNotFound {
		t.Fatalf("revoked card data still served: %d", r.Status)
	}
	// A new share is a new link.
	fresh := w.expect(w.do(http.MethodPost, w.cardPath(w.a1.InstallationID, "/share"), actor, nil), http.StatusOK, "reshare").JSON(t)
	if fresh["token"] == token {
		t.Fatal("re-sharing reused the revoked token")
	}
}

func TestPlayerCardAuthorization(t *testing.T) {
	w := newFactionWorld(t)
	unlinked, onlyA := w.players[2], w.players[3]
	p := w.linkPlayer(w.a1, onlyA, "CardOnlyA")
	w.insertKill(w.a1, p, time.Now(), false)
	for _, suffix := range []string{"", ".png"} {
		if r := w.do(http.MethodGet, w.cardPath(w.a1.InstallationID, suffix), unlinked, nil); r.Status == http.StatusOK {
			t.Fatalf("unlinked user got a card (%s)", suffix)
		}
		// Org B is another guild: no verified link there, so the stable identity error, never a card.
		if r := w.do(http.MethodGet, w.cardPath(w.b1.InstallationID, suffix), onlyA, nil); r.Status != http.StatusConflict || r.errCode(t) != codePlayerIdentityRequired {
			t.Fatalf("card on another tenant's server (%s): %d %s", suffix, r.Status, r.Body)
		}
	}
	if r := w.do(http.MethodPost, w.cardPath(w.b1.InstallationID, "/share"), onlyA, nil); r.Status != http.StatusConflict {
		t.Fatalf("share on another tenant's server: %d", r.Status)
	}
	// Same guild, second server the player was never observed on: non-enumerating 404.
	second := w.secondInstallation(w.a1)
	if r := w.do(http.MethodGet, w.cardPath(second.InstallationID, ""), onlyA, nil); r.Status != http.StatusNotFound {
		t.Fatalf("card on a server never played on: %d %s", r.Status, r.Body)
	}
	// The shared-card data route needs the service secret even though it needs no acting user.
	req, _ := http.NewRequest(http.MethodGet, w.base+"/api/saas/cards/anything", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("shared card data without service auth: %d", resp.StatusCode)
	}
}

func TestRenderedCardCacheIsBoundedAndExpires(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := &renderedCardCache{entries: map[string]renderedCard{}, now: func() time.Time { return now }}
	for i := 0; i < cardCacheEntries+50; i++ {
		c.put(fmt.Sprint("t", i), []byte{1})
		now = now.Add(time.Millisecond)
	}
	if len(c.entries) != cardCacheEntries {
		t.Fatalf("cache grew to %d entries", len(c.entries))
	}
	if _, ok := c.get("t0"); ok {
		t.Fatal("oldest entry survived eviction")
	}
	last := fmt.Sprint("t", cardCacheEntries+49)
	if _, ok := c.get(last); !ok {
		t.Fatal("newest entry missing")
	}
	now = now.Add(cardCacheTTL + time.Second)
	if _, ok := c.get(last); ok {
		t.Fatal("expired entry served")
	}
}
