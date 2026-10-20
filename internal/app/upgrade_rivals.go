package app

import (
	"net/http"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Upgrade 11, rivals and weapon mastery (Player Hub → My stats): the player's nemesis (who killed
// them most), favourite victim (whom they killed most) and kills by weapon with a mastery rank.
// Always on: it only shows the player their own record.

// weaponMasteryRanks are the kills with one weapon each rank needs.
var weaponMasteryRanks = []struct {
	Name  string
	Kills int
}{{"Bronze", 25}, {"Silver", 100}, {"Gold", 250}, {"Master", 500}}

type weaponMasteryDTO struct {
	repository.WeaponRecord
	Rank string `json:"rank"` // "" below Bronze
	Next string `json:"next,omitempty"`
	// ToNext is how many more kills the next rank needs (0 at Master).
	ToNext int `json:"toNext"`
}

// weaponMastery ranks a weapon record.
func weaponMastery(w repository.WeaponRecord) weaponMasteryDTO {
	out := weaponMasteryDTO{WeaponRecord: w}
	for _, r := range weaponMasteryRanks {
		if w.Kills >= r.Kills {
			out.Rank = r.Name
			continue
		}
		out.Next, out.ToNext = r.Name, r.Kills-w.Kills
		break
	}
	return out
}

func (a *App) registerRivalRoutes() {
	a.HTTPServer.Handle("GET /api/saas/player/servers/{installationID}/rivals", a.handlePlayerRivals)
}

// handlePlayerRivals is GET /api/saas/player/servers/{installationID}/rivals.
func (a *App) handlePlayerRivals(w http.ResponseWriter, r *http.Request) {
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	resp := struct {
		Nemesis         *repository.Rival  `json:"nemesis"`
		FavouriteVictim *repository.Rival  `json:"favouriteVictim"`
		Weapons         []weaponMasteryDTO `json:"weapons"`
	}{Weapons: []weaponMasteryDTO{}}
	if a.Upgrades == nil {
		writeSaaSJSON(w, http.StatusOK, resp)
		return
	}
	nemesis, victim, err := a.Upgrades.Rivals(ctx, scope.GuildID, scope.ServerID, scope.PlayerID)
	if err != nil {
		playerFailed(w, "rivals", err)
		return
	}
	weapons, err := a.Upgrades.WeaponRecords(ctx, scope.GuildID, scope.ServerID, scope.PlayerID, 8)
	if err != nil {
		playerFailed(w, "weapon records", err)
		return
	}
	resp.Nemesis, resp.FavouriteVictim = nemesis, victim
	for _, wr := range weapons {
		resp.Weapons = append(resp.Weapons, weaponMastery(wr))
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}
