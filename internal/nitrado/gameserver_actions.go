package nitrado

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Champion Client Admin Control Plane Phase 1 (docs/CLIENT_ADMIN.md "DayZ capability audit"):
// gameserver action endpoints, verified against Nitrado's own official PHP SDK
// (github.com/nitrado/NitrAPI-PHP, lib/Nitrapi/Services/Gameservers/{Gameserver,Whitelist,
// Banlist}.php) rather than assumed - the same caution this codebase applied to the delta-read
// investigation (docs/NITRADO_DELTA_READS.md) applies here: a capability with no first-party
// documented endpoint is reported UNSUPPORTED/DEFERRED, never guessed at with a fabricated call.
//
// Verified endpoints used below:
//   - POST /services/{id}/gameservers/restart  (optional form param: message)
//   - POST /services/{id}/gameservers/stop     (optional form param: message)
//   - whitelist and ban list: the settings.general "whitelist" and "bans" values (see below)
//
// Every call here follows this package's existing convention (logs.go, partial_read.go) of
// passing parameters as a URL query string with a nil body, rather than assuming a JSON or
// form-encoded body Nitrado's action endpoints were never confirmed to accept - client.do always
// sets Content-Type: application/json when a body is present, which would be wrong for these
// endpoints if they expect form encoding instead. A query string sidesteps that ambiguity exactly
// as the file-server endpoints already do.
//
// No method here has been exercised against a live Nitrado service in this environment (no
// credentials available) - callers must treat these as unverified-until-an-operator-confirms,
// exactly like NITRADO_DELTA_READ_MODE's rollout story.

// Restart requests a gameserver restart. message, if non-empty, is passed through as Nitrado's
// optional restart reason/message; it is never logged (may reflect user-entered admin reasoning).
func (c *Client) Restart(ctx context.Context, serviceID, message string) error {
	return c.gameserverAction(ctx, serviceID, "restart", message)
}

// Stop requests a gameserver stop. message is the optional stop reason, same caveats as Restart.
func (c *Client) Stop(ctx context.Context, serviceID, message string) error {
	return c.gameserverAction(ctx, serviceID, "stop", message)
}

func (c *Client) gameserverAction(ctx context.Context, serviceID, action, message string) error {
	if serviceID == "" {
		return fmt.Errorf("service ID is required")
	}
	q := url.Values{}
	if message != "" {
		q.Set("message", message)
	}
	path := "/services/" + url.PathEscape(serviceID) + "/gameservers/" + action
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	resp, err := c.do(ctx, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyStatus("gameserver_"+action, resp.StatusCode, KindNotFound)
	}
	return nil
}

// The whitelist and the ban list work like the priority list (priority.go): Nitrado has no
// add/remove endpoint for them. Each is one setting in settings.general ("whitelist", "bans")
// holding every player name on its own line, changed by writing the whole value back. An earlier
// version called /gameservers/games/banlist and /whitelist, which Nitrado answers with 501.
//
// identifier is the player's name as the game shows it (gamertag / PSN ID). Champion's own
// installation_access_entries table is the source of truth for reason/notes/expiry metadata;
// these calls are the Nitrado-side enforcement only.

// ErrAccessListUnsupported means the service's settings carry no such list.
var ErrAccessListUnsupported = errors.New("nitrado: this service has no such access list setting")

const (
	settingWhitelist = "whitelist"
	settingBans      = "bans"
)

func (c *Client) WhitelistAdd(ctx context.Context, serviceID, identifier string) error {
	return c.accessListChange(ctx, serviceID, settingWhitelist, identifier, true)
}

func (c *Client) WhitelistRemove(ctx context.Context, serviceID, identifier string) error {
	return c.accessListChange(ctx, serviceID, settingWhitelist, identifier, false)
}

func (c *Client) BanlistAdd(ctx context.Context, serviceID, identifier string) error {
	return c.accessListChange(ctx, serviceID, settingBans, identifier, true)
}

func (c *Client) BanlistRemove(ctx context.Context, serviceID, identifier string) error {
	return c.accessListChange(ctx, serviceID, settingBans, identifier, false)
}

// generalListSetting reads one one-name-per-line setting of settings.general. Only that key is
// decoded: the same body carries passwords, which never reach memory the caller can see.
func (c *Client) generalListSetting(ctx context.Context, serviceID, key string) ([]string, error) {
	var wire struct {
		Data struct {
			Gameserver struct {
				Settings struct {
					General map[string]json.RawMessage `json:"general"`
				} `json:"settings"`
			} `json:"gameserver"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/services/"+url.PathEscape(serviceID)+"/gameservers", "gameserver "+key+" list", &wire); err != nil {
		return nil, err
	}
	raw, ok := wire.Data.Gameserver.Settings.General[key]
	for k := range wire.Data.Gameserver.Settings.General {
		if k != key {
			delete(wire.Data.Gameserver.Settings.General, k)
		}
	}
	if !ok || string(raw) == "null" {
		return nil, ErrAccessListUnsupported
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, ErrAccessListUnsupported
	}
	return ParsePriorityList(value), nil
}

// accessListChange adds or removes one name: read the list, change it, write the whole list back.
// Nothing is written when the list could not be read first, or when it already is as wanted.
func (c *Client) accessListChange(ctx context.Context, serviceID, key, identifier string, add bool) error {
	if serviceID == "" {
		return fmt.Errorf("service ID is required")
	}
	identifier = strings.TrimSpace(identifier)
	if !ValidPriorityName(identifier) {
		return fmt.Errorf("identifier is required and must be a single line of at most 64 characters")
	}
	names, err := c.generalListSetting(ctx, serviceID, key)
	if err != nil {
		return err
	}
	next := make([]string, 0, len(names)+1)
	found := false
	for _, n := range names {
		if strings.EqualFold(n, identifier) {
			found = true
			if !add {
				continue
			}
		}
		next = append(next, n)
	}
	if add == found {
		return nil // already on the list, or already off it
	}
	if add {
		next = append(next, identifier)
	}
	if len(next) > MaxPriorityEntries {
		return fmt.Errorf("the list is limited to %d players", MaxPriorityEntries)
	}
	for _, n := range next {
		if !ValidPriorityName(n) {
			return fmt.Errorf("the list holds a name that cannot be written")
		}
	}
	body, err := json.Marshal(map[string]string{"category": "general", "key": key, "value": strings.Join(next, "\r\n")})
	if err != nil {
		return fmt.Errorf("encode %s list: %w", key, err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/services/"+url.PathEscape(serviceID)+"/gameservers/settings", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyStatus("gameserver_"+key, resp.StatusCode, KindNotFound)
	}
	return nil
}

// DeleteFile deletes one file through the file server:
//
//	DELETE /services/{id}/gameservers/file_server/delete?path=<full path>
//
// (Nitrado's official PHP SDK, FileServer.php deleteFile.) Like the action endpoints above it has
// NOT been exercised against a live Nitrado service: until an operator confirms it on a real
// server it is unverified, and its caller judges the result only by listing the folder again.
//
// It is called ONLY by internal/maprotation/charwipe, which may delete exactly one file,
// <mission folder>/storage_1/players.db, and refuses every other path before this is reached
// (docs/MAP_ROTATION.md; enforced by the isolation test in internal/shop/missionwrite). This
// method deletes whatever path it is given, so nothing else may call it.
func (c *Client) DeleteFile(ctx context.Context, serviceID, path string) error {
	if serviceID == "" || path == "" {
		return fmt.Errorf("service ID and path are required")
	}
	c.forgetListings()
	defer c.forgetListings()
	q := url.Values{"path": {path}}
	resp, err := c.do(ctx, http.MethodDelete, "/services/"+url.PathEscape(serviceID)+"/gameservers/file_server/delete?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyStatus("file_server_delete", resp.StatusCode, KindNotFound)
	}
	return nil
}
