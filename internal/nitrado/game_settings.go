package nitrado

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Game settings: the on/off switches of Nitrado's own web panel (storage wipe, whitelist, base
// damage, respawn rules, ...) that the website offers under Nitrado controls.
//
// Nitrado keeps them as settings of the gameserver read, data.gameserver.settings.<category>.<key>
// ("config" mirrors serverDZ.cfg, "general" holds Nitrado's own lists and switches), and changes
// them the same way as the priority list (priority.go):
//
//	GET  /services/{id}/gameservers           -> data.gameserver.settings.<category>.<key>
//	POST /services/{id}/gameservers/settings  body: {"category":"...","key":"...","value":"1"|"0"}
//
// The exact key and category of each switch is not documented, so they are DISCOVERED: for each
// catalogue entry the read looks through every category for the first candidate key that exists,
// case-sensitively first and then ignoring case. A switch whose key is absent on the service is
// reported Supported=false and is never written. Boolean values arrive as strings: "1", "true",
// "on" and "yes" are on, everything else is off; writes send "1" or "0". Nitrado applies most of
// them when the server next restarts.
//
// Only the catalogue's candidate keys are decoded into typed values: the same body carries
// passwords, which never reach memory the caller can see and are never logged.

// GameSettingSpec describes one switch of the catalogue.
type GameSettingSpec struct {
	// ID is the stable id the website and the audit log use (snake_case).
	ID string
	// Label and Help are Nitrado's own panel wording.
	Label string
	Help  string
	// Keys are the candidate setting keys, most likely first. Discovery stops at the first that
	// exists on the service.
	Keys []string
	// Destructive marks a switch that destroys data (the storage wipe): the app layer demands a
	// stronger capability and a typed confirmation before enabling it.
	Destructive bool
}

// GameSettingCatalogue lists the supported switches in the order the website shows them.
var GameSettingCatalogue = []GameSettingSpec{
	{ID: "wipe_on_next_restart", Label: "Storage wipe", Help: "Wipes storage (player and world data) on the next restart.", Keys: []string{"wipe_on_next_restart"}, Destructive: true},
	{ID: "mouse_and_keyboard", Label: "Mouse & keyboard", Help: "Also allow players to use keyboard and mouse.", Keys: []string{"enableMouseAndKeyboard", "mouseAndKeyboard", "enable_mouse_and_keyboard"}},
	{ID: "whitelist", Label: "Whitelist", Help: "Activate the whitelist.", Keys: []string{"enableWhitelist", "whitelist_enabled", "enable_whitelist", "enableWhiteList"}},
	{ID: "disable_base_damage", Label: "Disable base damage", Help: "Turn on to disable damage/destruction of fences and watchtowers.", Keys: []string{"disableBaseDamage"}},
	{ID: "disable_container_damage", Label: "Disable container damage", Help: "Turn on to disable damage/destruction of tents, barrels, wooden crates and sea chests.", Keys: []string{"disableContainerDamage"}},
	{ID: "disable_respawn_dialog", Label: "Disable respawn dialog", Help: "Spawn with a random character or the character from the main menu.", Keys: []string{"disableRespawnDialog"}},
	{ID: "disable_respawn_in_unconsciousness", Label: "Disable respawning in unconsciousness", Help: "Disable the Respawn button in the pause menu while the player is unconscious.", Keys: []string{"disableRespawnInUnconsciousness"}},
	{ID: "disable_multi_account_mitigation", Label: "Disable multi-account mitigation", Help: "Turn on to disable the multi-account mitigation.", Keys: []string{"disableMultiAccountMitigation"}},
}

// GameSettingSpecByID returns the catalogue entry with id, if there is one.
func GameSettingSpecByID(id string) (GameSettingSpec, bool) {
	for _, spec := range GameSettingCatalogue {
		if spec.ID == id {
			return spec, true
		}
	}
	return GameSettingSpec{}, false
}

// GameSetting is one switch as discovered on a service.
type GameSetting struct {
	ID string
	// Supported is false when none of the candidate keys exists on the service; Category, Key and
	// Enabled are then zero.
	Supported bool
	Enabled   bool
	// Category and Key are what the service really carries (a case-insensitive match keeps the
	// service's spelling), and what a write must use.
	Category string
	Key      string
}

// ParseGameSettingValue reads Nitrado's string booleans: "1", "true", "on" and "yes" (any case,
// surrounding space ignored) are on, everything else is off.
func ParseGameSettingValue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// gameSettingValue decodes one raw setting value into on/off. Nitrado sends strings; a bare JSON
// bool or number is accepted too, anything else (null, objects) counts as off.
func gameSettingValue(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return false
	}
	switch t[0] {
	case '"':
		var s string
		if err := json.Unmarshal(t, &s); err != nil {
			return false
		}
		return ParseGameSettingValue(s)
	case 't', 'f':
		var b bool
		if err := json.Unmarshal(t, &b); err != nil {
			return false
		}
		return b
	default:
		if n, err := strconv.ParseFloat(string(t), 64); err == nil {
			return n != 0
		}
		return false
	}
}

// discoverGameSettings applies the catalogue's discovery rule to the settings of one service:
// categories and their keys only (the values are consulted only for the discovered keys). The
// category order is alphabetical, so a key present in two categories resolves the same way on
// every read.
func discoverGameSettings(settings map[string]map[string]json.RawMessage) []GameSetting {
	categories := make([]string, 0, len(settings))
	for name := range settings {
		categories = append(categories, name)
	}
	sort.Strings(categories)

	find := func(candidate string, fold bool) (category, key string, raw json.RawMessage, ok bool) {
		for _, cat := range categories {
			keys := settings[cat]
			if !fold {
				if v, present := keys[candidate]; present {
					return cat, candidate, v, true
				}
				continue
			}
			names := make([]string, 0, len(keys))
			for k := range keys {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				if strings.EqualFold(k, candidate) {
					return cat, k, keys[k], true
				}
			}
		}
		return "", "", nil, false
	}

	out := make([]GameSetting, 0, len(GameSettingCatalogue))
	for _, spec := range GameSettingCatalogue {
		item := GameSetting{ID: spec.ID}
		for _, candidate := range spec.Keys {
			cat, key, raw, ok := find(candidate, false)
			if !ok {
				cat, key, raw, ok = find(candidate, true)
			}
			if ok {
				item.Supported, item.Category, item.Key, item.Enabled = true, cat, key, gameSettingValue(raw)
				break
			}
		}
		out = append(out, item)
	}
	return out
}

// GameSettings reads the service's switches, one per catalogue entry in catalogue order. Only the
// catalogue's candidate keys are looked at; every other setting (passwords among them) is dropped
// as soon as the body is decoded and never logged.
func (c *Client) GameSettings(ctx context.Context, serviceID string) ([]GameSetting, error) {
	if serviceID == "" {
		return nil, fmt.Errorf("service ID is required")
	}
	var wire struct {
		Data struct {
			Gameserver struct {
				Settings map[string]map[string]json.RawMessage `json:"settings"`
			} `json:"gameserver"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/services/"+url.PathEscape(serviceID)+"/gameservers", "gameserver game settings", &wire); err != nil {
		return nil, err
	}
	settings := wire.Data.Gameserver.Settings
	for cat, keys := range settings {
		for k := range keys {
			if !isGameSettingCandidate(k) {
				delete(keys, k)
			}
		}
		if len(keys) == 0 {
			delete(settings, cat)
		}
	}
	return discoverGameSettings(settings), nil
}

// isGameSettingCandidate reports whether key (ignoring case) is a candidate key of any catalogue entry.
func isGameSettingCandidate(key string) bool {
	for _, spec := range GameSettingCatalogue {
		for _, candidate := range spec.Keys {
			if strings.EqualFold(candidate, key) {
				return true
			}
		}
	}
	return false
}

// SetGameSetting writes one switch as "1" or "0". category and key must be what GameSettings
// discovered on the same service a moment ago: only a catalogue candidate key is ever written.
func (c *Client) SetGameSetting(ctx context.Context, serviceID, category, key string, enabled bool) error {
	if serviceID == "" {
		return fmt.Errorf("service ID is required")
	}
	if category == "" || key == "" {
		return fmt.Errorf("the setting's category and key are required")
	}
	if !isGameSettingCandidate(key) {
		return fmt.Errorf("%q is not a game setting Champion writes", key)
	}
	value := "0"
	if enabled {
		value = "1"
	}
	body, err := json.Marshal(map[string]string{"category": category, "key": key, "value": value})
	if err != nil {
		return fmt.Errorf("encode game setting: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/services/"+url.PathEscape(serviceID)+"/gameservers/settings", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyStatus("gameserver_game_setting", resp.StatusCode, KindNotFound)
	}
	return nil
}
