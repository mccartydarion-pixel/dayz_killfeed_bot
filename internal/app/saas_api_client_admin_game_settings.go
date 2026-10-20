package app

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/permissions"
)

// --- game settings (Nitrado's own switches: storage wipe, whitelist, base damage, respawn rules) ---
//
// Nitrado holds each switch as one setting and is the only source of truth: Champion keeps no
// copy, so a switch flipped in Nitrado's own panel shows here and the other way round. The exact
// key and category of each switch are discovered on every read (internal/nitrado/game_settings.go);
// a switch the service does not carry is reported unsupported and is never written. Nitrado
// applies most of them when the server next restarts.

// gameSettingAppliesOn is when Nitrado applies a changed switch: at the next server restart.
const gameSettingAppliesOn = "restart"

type gameSettingDTO struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Help        string `json:"help"`
	Supported   bool   `json:"supported"`
	Enabled     bool   `json:"enabled"`
	Category    string `json:"category"`
	Key         string `json:"key"`
	Destructive bool   `json:"destructive"`
	AppliesOn   string `json:"appliesOn"`
}

type gameSettingsDTO struct {
	Items []gameSettingDTO `json:"items"`
}

type setGameSettingRequest struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason"`
	Confirm string `json:"confirm"`
}

// gameSettingConfirmWord is what the request must carry to enable the storage wipe.
const gameSettingConfirmWord = "WIPE"

func toGameSettingDTO(spec nitrado.GameSettingSpec, s nitrado.GameSetting) gameSettingDTO {
	return gameSettingDTO{
		ID: spec.ID, Label: spec.Label, Help: spec.Help,
		Supported: s.Supported, Enabled: s.Enabled, Category: s.Category, Key: s.Key,
		Destructive: spec.Destructive, AppliesOn: gameSettingAppliesOn,
	}
}

// findGameSetting picks id out of a GameSettings read.
func findGameSetting(items []nitrado.GameSetting, id string) (nitrado.GameSetting, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return nitrado.GameSetting{}, false
}

func (a *App) handleListGameSettings(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapServerSettingsManage)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	client, serviceID, ok := a.nitradoClientForOrg(ctx, w, ac)
	if !ok {
		return
	}
	items, err := client.GameSettings(ctx, serviceID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "nitrado game settings read failed", "err", err.Error())
		writeSaaSError(w, codeNitradoUnavailable, "could not read the game settings from Nitrado")
		return
	}
	out := make([]gameSettingDTO, 0, len(nitrado.GameSettingCatalogue))
	for _, spec := range nitrado.GameSettingCatalogue {
		s, _ := findGameSetting(items, spec.ID)
		out = append(out, toGameSettingDTO(spec, s))
	}
	writeSaaSJSON(w, http.StatusOK, gameSettingsDTO{Items: out})
}

func (a *App) handleSetGameSetting(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapServerSettingsManage)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	spec, known := nitrado.GameSettingSpecByID(id)
	if !known {
		writeSaaSError(w, codeNotFound, "unknown game setting")
		return
	}
	// The storage wipe destroys every player's and the world's data: the OWNER's own capability,
	// in addition to SERVER_SETTINGS_MANAGE, whichever way it is being flipped.
	if spec.Destructive && !permissions.Allows(ac.level, permissions.CapServerWipe) {
		writeSaaSError(w, codeAdminForbidden, "missing required permission: "+string(permissions.CapServerWipe))
		return
	}
	req, ok := decodeJSONBody[setGameSettingRequest](w, r)
	if !ok {
		return
	}
	// Enabling the wipe needs the typed word; turning it off again does not.
	if spec.Destructive && req.Enabled && strings.TrimSpace(req.Confirm) != gameSettingConfirmWord {
		writeSaaSError(w, codeInvalidRequest, "type "+gameSettingConfirmWord+" to confirm")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	client, serviceID, ok := a.nitradoClientForOrg(ctx, w, ac)
	if !ok {
		return
	}
	// The read guards the write: nothing is written to a service whose switch could not be read
	// first, and the write uses the category and key the read discovered.
	items, err := client.GameSettings(ctx, serviceID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "nitrado game settings read failed", "err", err.Error())
		writeSaaSError(w, codeNitradoUnavailable, "could not read the game settings from Nitrado")
		return
	}
	current, _ := findGameSetting(items, spec.ID)
	if !current.Supported {
		writeSaaSError(w, codeInvalidRequest, "Nitrado does not offer this setting on this server")
		return
	}
	before := map[string]bool{"enabled": current.Enabled}
	after := map[string]bool{"enabled": req.Enabled}
	// One process-wide lock per service and setting, as every other Nitrado setting write takes.
	unlock := nitrado.LockSetting(serviceID, current.Key)
	err = client.SetGameSetting(ctx, serviceID, current.Category, current.Key, req.Enabled)
	if err != nil {
		unlock()
		a.recordAudit(ctx, ac, "GAME_SETTING_SET", spec.ID, req.Reason, "failure", before, after)
		slog.Warn("component=saas_api", "msg", "nitrado game setting write failed", "setting", spec.ID, "err", err.Error())
		writeSaaSError(w, codeNitradoUnavailable, "Nitrado rejected this request")
		return
	}
	// Answer with what Nitrado now reports, or with the written value when the re-read fails.
	written := current
	written.Enabled = req.Enabled
	if refreshed, err := client.GameSettings(ctx, serviceID); err == nil {
		if got, found := findGameSetting(refreshed, spec.ID); found && got.Supported {
			written = got
		}
	} else {
		slog.Warn("component=saas_api", "msg", "nitrado game settings re-read failed", "setting", spec.ID, "err", err.Error())
	}
	unlock()
	a.recordAudit(ctx, ac, "GAME_SETTING_SET", spec.ID, req.Reason, "success", before, after)
	writeSaaSJSON(w, http.StatusOK, toGameSettingDTO(spec, written))
}
