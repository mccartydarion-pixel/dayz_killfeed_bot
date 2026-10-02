//go:build integration

package app

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The website's player search (Players workspace, "Give a player a tier") reads accountId and
// gamertag from this route. Rows written without a DTO came out under Go field names, so every
// result was a nameless player with no id.
func TestClientAdminAccountSearchUsesTheEconomyWireShape(t *testing.T) {
	w := newClientAdminWorld(t)
	if w.a.EconomyAccounts == nil {
		repo := repository.NewEconomyRepository(w.a.DB.Pool)
		w.a.EconomyAccounts = economy.NewAccounts(economy.NewService(repo, nil), repo)
	}
	name := fmt.Sprintf("Ceiyxe%d", time.Now().UnixNano()%1_000_000)
	playerID := w.seedPlayer(name)

	rr := w.call(w.a.handleAdminEconomyAccounts, http.MethodGet, w.path("/economy/accounts?q="+name), w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("account search failed: %d %s", rr.Code, rr.Body.String())
	}
	body := decodeBody[struct {
		Items []map[string]any `json:"items"`
	}](t, rr)
	if len(body.Items) != 1 {
		t.Fatalf("expected the seeded player, got %s", rr.Body.String())
	}
	item := body.Items[0]
	if id, _ := item["accountId"].(float64); int64(id) != playerID {
		t.Fatalf("accountId must be the player id %d, got %v in %s", playerID, item["accountId"], rr.Body.String())
	}
	if item["gamertag"] != name {
		t.Fatalf("gamertag must be %q, got %v", name, item["gamertag"])
	}
	for _, goName := range []string{"AccountID", "Gamertag", "DisplayName"} {
		if _, leaked := item[goName]; leaked {
			t.Fatalf("Go field name %s leaked into the response: %s", goName, rr.Body.String())
		}
	}
}
