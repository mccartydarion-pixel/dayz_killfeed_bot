package nitrado

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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
//   - POST/DELETE /services/{id}/gameservers/games/whitelist  (form param: identifier)
//   - POST/DELETE /services/{id}/gameservers/games/banlist    (form param: identifier)
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

// WhitelistAdd/WhitelistRemove and BanlistAdd/BanlistRemove manage Nitrado's own DayZ
// whitelist/ban list by player identifier (a platform-specific player ID, never a display name -
// Nitrado's API has no notion of a display name). Champion's own installation_access_entries
// table is the source of truth for reason/notes/expiry metadata; these calls are the Nitrado-side
// enforcement action only.

func (c *Client) WhitelistAdd(ctx context.Context, serviceID, identifier string) error {
	return c.accessListAction(ctx, serviceID, "whitelist", http.MethodPost, identifier)
}

func (c *Client) WhitelistRemove(ctx context.Context, serviceID, identifier string) error {
	return c.accessListAction(ctx, serviceID, "whitelist", http.MethodDelete, identifier)
}

func (c *Client) BanlistAdd(ctx context.Context, serviceID, identifier string) error {
	return c.accessListAction(ctx, serviceID, "banlist", http.MethodPost, identifier)
}

func (c *Client) BanlistRemove(ctx context.Context, serviceID, identifier string) error {
	return c.accessListAction(ctx, serviceID, "banlist", http.MethodDelete, identifier)
}

func (c *Client) accessListAction(ctx context.Context, serviceID, list, method, identifier string) error {
	if serviceID == "" {
		return fmt.Errorf("service ID is required")
	}
	if identifier == "" {
		return fmt.Errorf("identifier is required")
	}
	q := url.Values{"identifier": {identifier}}
	path := "/services/" + url.PathEscape(serviceID) + "/gameservers/games/" + list + "?" + q.Encode()
	resp, err := c.do(ctx, method, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyStatus("gameserver_"+list, resp.StatusCode, KindNotFound)
	}
	return nil
}
