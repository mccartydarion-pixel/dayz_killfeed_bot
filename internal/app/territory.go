package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/progression"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Territory control (docs/PROGRESSION.md "Territory").

const (
	territoryColor = 0xF97316
	// territoryKillLookback is how far back each pass places kills in zones (late kills included).
	territoryKillLookback = 3 * time.Hour
	territoryKillBatch    = 5000
)

func territoryAuthor() *discordgo.MessageEmbedAuthor {
	return &discordgo.MessageEmbedAuthor{Name: "CHAMPIONS® TERRITORY"}
}

// territoryZoneDTO is one zone with its holder and the race for it.
type territoryZoneDTO struct {
	progression.Zone
	Holder    *repository.TerritoryFaction `json:"holder"`
	HeldSince *string                      `json:"heldSince,omitempty"`
	// Standings are the factions with points in the window, best first (staff and players only).
	Standings []territoryStandingDTO `json:"standings,omitempty"`
	// MyPoints is the acting player's faction's points (player route only).
	MyPoints *int `json:"myPoints,omitempty"`
}

type territoryStandingDTO struct {
	Faction repository.TerritoryFaction `json:"faction"`
	Points  int                         `json:"points"`
}

// territoryZones builds the zones of a server with holders and (withStandings) the scores.
func (a *App) territoryZones(ctx context.Context, ts repository.TerritoryServer, now time.Time, withStandings bool) ([]territoryZoneDTO, map[string][]progression.FactionScore, error) {
	holds, err := a.Territory.Holds(ctx, ts.ServerID)
	if err != nil {
		return nil, nil, err
	}
	standings := map[string][]progression.FactionScore{}
	if withStandings {
		if standings, err = a.Territory.Standings(ctx, ts.ServerID, now.AddDate(0, 0, -ts.Settings.WindowDays)); err != nil {
			return nil, nil, err
		}
	}
	ids := []int64{}
	for _, h := range holds {
		ids = append(ids, h.FactionID)
	}
	for _, list := range standings {
		for _, s := range list {
			ids = append(ids, s.FactionID)
		}
	}
	factions, err := a.Territory.Factions(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	out := []territoryZoneDTO{}
	for _, z := range progression.Zones(ts.MapKey) {
		dto := territoryZoneDTO{Zone: z}
		if h, ok := holds[z.Key]; ok {
			if f, ok := factions[h.FactionID]; ok {
				dto.Holder = &f
				since := rfc3339(h.Since)
				dto.HeldSince = &since
			}
		}
		list := append([]progression.FactionScore(nil), standings[z.Key]...)
		sort.Slice(list, func(i, j int) bool {
			if list[i].Points != list[j].Points {
				return list[i].Points > list[j].Points
			}
			return list[i].FactionID < list[j].FactionID
		})
		for i, s := range list {
			if i == 5 {
				break
			}
			if f, ok := factions[s.FactionID]; ok {
				dto.Standings = append(dto.Standings, territoryStandingDTO{Faction: f, Points: s.Points})
			}
		}
		out = append(out, dto)
	}
	return out, standings, nil
}

func (a *App) handleAdminTerritory(w http.ResponseWriter, r *http.Request) {
	_, serverID, ok := a.progressionAdmin(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	ts, err := a.Territory.Server(ctx, serverID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load territory")
		return
	}
	zones, _, err := a.territoryZones(ctx, ts, time.Now().UTC(), true)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load territory")
		return
	}
	captures, _ := a.Territory.RecentCaptures(ctx, serverID, 15)
	writeSaaSJSON(w, http.StatusOK, map[string]any{"settings": ts.Settings, "map": liveMapMap(ts.MapKey), "zones": zones, "captures": captures})
}

func (a *App) handleSaveTerritory(w http.ResponseWriter, r *http.Request) {
	ac, serverID, ok := a.progressionAdmin(w, r, true)
	if !ok {
		return
	}
	req, ok := decodeJSONBody[repository.TerritorySettings](w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	before, _ := a.Territory.Settings(ctx, serverID)
	saved, err := a.Territory.SaveSettings(ctx, serverID, req, ac.user.DiscordUserID, time.Now().UTC())
	if errors.Is(err, repository.ErrTerritorySettingsInvalid) {
		writeSaaSError(w, codeInvalidRequest, "the window is 1 to 14 days, a capture needs 1 to 50 kills and income is 0 to 1,000,000 points a zone a day")
		return
	}
	if err != nil {
		progressionWarn("territory settings save", serverID, err)
		writeSaaSError(w, codeInternalError, "could not save territory")
		return
	}
	a.recordAudit(ctx, ac, "TERRITORY_SAVE", fmt.Sprintf("server:%d", serverID), "", "success", before, saved)
	a.invalidateLiveMapCache()
	writeSaaSJSON(w, http.StatusOK, saved)
}

// handlePlayerTerritory is GET .../player/servers/{installationID}/territory: every zone, who holds
// it, the race for it and the player's faction's points.
func (a *App) handlePlayerTerritory(w http.ResponseWriter, r *http.Request) {
	pc, ok := a.progressionPlayer(w, r, false)
	if !ok {
		return
	}
	defer pc.cancel()
	ctx, scope := pc.ctx, pc.scope
	ts, err := a.Territory.Server(ctx, scope.ServerID)
	if err != nil {
		playerFailed(w, "territory", err)
		return
	}
	resp := map[string]any{"enabled": ts.Settings.Enabled}
	if !ts.Settings.Enabled {
		writeSaaSJSON(w, http.StatusOK, resp)
		return
	}
	zones, standings, err := a.territoryZones(ctx, ts, time.Now().UTC(), true)
	if err != nil {
		playerFailed(w, "territory", err)
		return
	}
	mine, err := a.Territory.PlayerFaction(ctx, ts, scope.PlayerID)
	if err != nil {
		playerFailed(w, "territory faction", err)
		return
	}
	held := 0
	if mine != 0 {
		for i := range zones {
			pts := 0
			for _, s := range standings[zones[i].Key] {
				if s.FactionID == mine {
					pts = s.Points
				}
			}
			zones[i].MyPoints = &pts
			if zones[i].Holder != nil && zones[i].Holder.ID == mine {
				held++
			}
		}
		if f, err := a.Territory.Factions(ctx, []int64{mine}); err == nil {
			if fac, ok := f[mine]; ok {
				resp["faction"] = fac
			}
		}
	}
	resp["map"] = liveMapMap(ts.MapKey)
	resp["zones"] = zones
	resp["held"] = held
	resp["rules"] = map[string]any{"windowDays": ts.Settings.WindowDays, "minKills": ts.Settings.MinKills, "incomePoints": ts.Settings.IncomePoints,
		"activeDays": repository.TerritoryMemberActiveDays}
	writeSaaSJSON(w, http.StatusOK, resp)
}

// publicTerritory is the territory layer of the public live map: zones and holders, no scores.
func (a *App) publicTerritory(ctx context.Context, serverID int64, now time.Time) []territoryZoneDTO {
	if a.Territory == nil {
		return nil
	}
	ts, err := a.Territory.Server(ctx, serverID)
	if err != nil || !ts.Settings.Enabled {
		return nil
	}
	zones, _, err := a.territoryZones(ctx, ts, now, false)
	if err != nil {
		return nil
	}
	return zones
}

// runTerritory places new kills in zones, applies captures and pays the daily income.
func (a *App) runTerritory(ctx context.Context, guildID int64, now time.Time) {
	servers, err := a.Territory.EnabledServers(ctx, guildID)
	if err != nil {
		slog.Warn("component=progression", "msg", "territory servers failed", "err", err.Error())
		return
	}
	for _, ts := range servers {
		if ts.InstallationID == 0 || !a.upgradeRuns.due(fmt.Sprintf("territory:%d", ts.ServerID), territoryEvery, now) {
			continue
		}
		if err := a.placeTerritoryKills(ctx, ts, now); err != nil {
			progressionWarn("territory kills", ts.ServerID, err)
			continue
		}
		changed := a.applyCaptures(ctx, ts, now)
		a.payTerritoryIncome(ctx, ts, now)
		if changed {
			a.invalidateLiveMapCache()
		}
	}
}

func (a *App) placeTerritoryKills(ctx context.Context, ts repository.TerritoryServer, now time.Time) error {
	kills, err := a.Territory.RecentKills(ctx, ts.GuildID, ts.ServerID, now.Add(-territoryKillLookback), now.Add(time.Minute), territoryKillBatch)
	if err != nil {
		return err
	}
	zones := progression.Zones(ts.MapKey)
	placed := []repository.TerritoryKill{}
	for _, k := range kills {
		x, z := k.KillerX, k.KillerZ
		if x == nil || z == nil {
			x, z = k.VictimX, k.VictimZ
		}
		if x == nil || z == nil {
			continue
		}
		if zn, ok := progression.ZoneAt(zones, *x, *z); ok {
			placed = append(placed, repository.TerritoryKill{KillID: k.ID, ZoneKey: zn.Key, KillerID: k.KillerID, VictimID: k.VictimID, At: k.At})
		}
	}
	_, err = a.Territory.RecordKills(ctx, ts, placed)
	return err
}

// applyCaptures runs the capture rule on every zone; true when a zone changed hands.
func (a *App) applyCaptures(ctx context.Context, ts repository.TerritoryServer, now time.Time) bool {
	holds, err := a.Territory.Holds(ctx, ts.ServerID)
	if err != nil {
		progressionWarn("territory holds", ts.ServerID, err)
		return false
	}
	standings, err := a.Territory.Standings(ctx, ts.ServerID, now.AddDate(0, 0, -ts.Settings.WindowDays))
	if err != nil {
		progressionWarn("territory standings", ts.ServerID, err)
		return false
	}
	changed := false
	serverName := a.serverName(ts.ServerID)
	for _, z := range progression.Zones(ts.MapKey) {
		current := holds[z.Key].FactionID
		next := progression.DecideHolder(current, standings[z.Key], ts.Settings.MinKills)
		if next == current {
			continue
		}
		ok, err := a.Territory.ChangeHolder(ctx, ts.ServerID, z.Key, current, next, now)
		if err != nil {
			progressionWarn("territory capture", ts.ServerID, err)
			continue
		}
		if !ok {
			continue
		}
		changed = true
		slog.Info("component=progression", "event", "territory_changed", "server_id", ts.ServerID, "zone", z.Key, "from", current, "to", next)
		if ts.Settings.Announce {
			factions, err := a.Territory.Factions(ctx, []int64{current, next})
			if err == nil {
				points := 0
				for _, s := range standings[z.Key] {
					if s.FactionID == next {
						points = s.Points
					}
				}
				a.postProgressionCard(ctx, ts.GuildID, ts.ServerID, buildTerritoryCard(z, factions[current], factions[next], points, ts.Settings, serverName))
			}
		}
	}
	return changed
}

// payTerritoryIncome pays each held zone once a UTC day, at the first pass it is held that day: the
// zone's income is split between the holder's members who played in the last week. Each credit is
// idempotent per player, zone and day, so a pass that fails half-way is finished by the next one.
func (a *App) payTerritoryIncome(ctx context.Context, ts repository.TerritoryServer, now time.Time) {
	if ts.Settings.IncomePoints <= 0 || a.EconomyService == nil {
		return
	}
	day := now.UTC().Truncate(24 * time.Hour)
	members, err := a.Territory.IncomeMembers(ctx, ts, day, now)
	if err != nil {
		progressionWarn("territory income members", ts.ServerID, err)
		return
	}
	type zonePay struct {
		faction int64
		players []int64
	}
	byZone := map[string]*zonePay{}
	for _, m := range members {
		if byZone[m.ZoneKey] == nil {
			byZone[m.ZoneKey] = &zonePay{faction: m.FactionID}
		}
		byZone[m.ZoneKey].players = append(byZone[m.ZoneKey].players, m.PlayerID)
	}
	names := map[string]string{}
	for _, z := range progression.Zones(ts.MapKey) {
		names[z.Key] = z.Name
	}
	for zone, zp := range byZone {
		share := progression.IncomeShares(ts.Settings.IncomePoints, len(zp.players))
		paid := true
		for _, player := range zp.players {
			if _, err := a.EconomyService.Credit(ctx, economy.Request{GuildID: ts.GuildID, ServerID: ts.ServerID, PlayerID: player, Amount: share,
				Type: economy.TypeSystemReward, ReferenceID: fmt.Sprintf("territory:%d:%s:%s", ts.ServerID, day.Format("2006-01-02"), zone),
				Description: "Territory income: " + names[zone]}); err != nil {
				progressionWarn("territory income", ts.ServerID, err)
				paid = false
				break
			}
		}
		if !paid {
			continue
		}
		if err := a.Territory.RecordPayout(ctx, ts.ServerID, day, zone, zp.faction, len(zp.players), share*int64(len(zp.players)), now); err != nil {
			progressionWarn("territory payout record", ts.ServerID, err)
			continue
		}
		slog.Info("component=progression", "event", "territory_income_paid", "server_id", ts.ServerID, "zone", zone, "players", len(zp.players), "share", share)
	}
}

func factionLabel(f repository.TerritoryFaction) string {
	if strings.TrimSpace(f.Tag) != "" {
		return fmt.Sprintf("[%s] %s", f.Tag, f.Name)
	}
	return f.Name
}

func buildTerritoryCard(z progression.Zone, from, to repository.TerritoryFaction, points int, s repository.TerritorySettings, serverName string) *discordgo.MessageEmbed {
	where := ""
	if strings.TrimSpace(serverName) != "" {
		where = " on " + serverName
	}
	if to.ID == 0 {
		return &discordgo.MessageEmbed{Author: territoryAuthor(), Color: 0x6B7280, Title: "🏳️ " + z.Name + " is unclaimed",
			Description: fmt.Sprintf("%s held %s%s but got no kills there in %d days. It's up for grabs.", factionLabel(from), z.Name, where, s.WindowDays)}
	}
	desc := fmt.Sprintf("**%s** took **%s**%s with %d kills there in the last %d days.", factionLabel(to), z.Name, where, points, s.WindowDays)
	if from.ID != 0 {
		desc = fmt.Sprintf("**%s** took **%s** from **%s**%s with %d kills there in the last %d days.", factionLabel(to), z.Name, factionLabel(from), where, points, s.WindowDays)
	}
	if s.IncomePoints > 0 {
		desc += fmt.Sprintf("\nIt pays the holders %s a day.", pointsText(s.IncomePoints))
	}
	return &discordgo.MessageEmbed{Author: territoryAuthor(), Color: territoryColor, Title: "🏴 " + z.Name + " captured", Description: desc}
}

// heatmapMapOf is the DayZ map a server runs, for the heatmap picture: the installation's map, else
// Chernarus (the live map's fallback).
func (a *App) heatmapMapOf(ctx context.Context, serverID int64) (string, float64, bool) {
	key := ""
	if a.Territory != nil {
		if ts, err := a.Territory.Server(ctx, serverID); err == nil {
			key = ts.MapKey
		}
	}
	m := liveMapMap(key)
	return m.Key, float64(m.Size), m.Size > 0
}
