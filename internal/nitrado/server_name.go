package nitrado

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The server's name as its owner set it on Nitrado (docs/SERVER_NAME_SYNC.md).
//
// Champion shows one name per game server. Unless the owner typed a name in Champion, that name
// follows Nitrado. Nitrado reports a name in four places; ServerName is the ONE order every caller
// uses (both connect flows and the periodic sync), so a sync right after connecting never flips
// the name:
//
//  1. settings.config.hostname of GET /services/:id/gameservers - the DayZ "hostname" setting, the
//     field the owner edits in the Nitrado panel. It changes as soon as it is saved.
//  2. query.server_name of the same response - the name the running server announces. It only
//     exists while the server is up and follows the setting after a restart.
//  3. details.name of GET /services - the service list's name.
//  4. details.server_name of GET /services - a legacy spelling older payloads carried.

// MaxServerNameLen caps a server name (bytes), the same limit the owner rename endpoint applies.
const MaxServerNameLen = 100

// CleanServerName makes a provider-supplied name safe to store and show: control characters
// become spaces, runs of whitespace collapse to one space, the ends are trimmed and the result is
// cut to MaxServerNameLen bytes without splitting a character. Invalid UTF-8 yields "".
func CleanServerName(s string) string {
	if !utf8.ValidString(s) {
		return ""
	}
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		if b.Len()+utf8.RuneLen(r) > MaxServerNameLen {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// GameserverName is the two name fields of GET /services/:id/gameservers. The same body carries
// passwords; only these fields are decoded.
type GameserverName struct {
	ServiceID int64  // data.gameserver.service_id; 0 when Nitrado omitted it
	Hostname  string // settings.config.hostname
	QueryName string // query.server_name ("" while the server is not answering queries)
}

// BelongsTo reports whether the response is provably for serviceID.
func (g GameserverName) BelongsTo(serviceID string) bool {
	return g.ServiceID > 0 && strconv.FormatInt(g.ServiceID, 10) == serviceID
}

// ServerName picks the server's Nitrado name from what was read, already cleaned, or "" when
// Nitrado reported none. Pass a zero GameserverName or Service for a source that was not read.
func ServerName(gs GameserverName, svc Service) string {
	for _, candidate := range []string{gs.Hostname, gs.QueryName, svc.Details.Name, svc.Details.ServerName} {
		if name := CleanServerName(candidate); name != "" {
			return name
		}
	}
	return ""
}

// jsonObjectString returns the string at key of a JSON object, or "" when raw is not an object
// (Nitrado serializes an empty object as []), the key is missing or its value is not a string.
func jsonObjectString(raw json.RawMessage, key string) string {
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(obj[key], &s); err != nil {
		return ""
	}
	return s
}

// GameserverName reads the name fields of GET /services/:id/gameservers. One GET, nothing else.
func (c *Client) GameserverName(ctx context.Context, serviceID string) (GameserverName, error) {
	var wire struct {
		Data struct {
			Gameserver struct {
				ServiceID int64           `json:"service_id"`
				Query     json.RawMessage `json:"query"`
				Settings  json.RawMessage `json:"settings"`
			} `json:"gameserver"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/services/"+url.PathEscape(serviceID)+"/gameservers", "gameserver name", &wire); err != nil {
		return GameserverName{}, err
	}
	g := wire.Data.Gameserver
	out := GameserverName{ServiceID: g.ServiceID, QueryName: jsonObjectString(g.Query, "server_name")}
	if t := bytes.TrimSpace(g.Settings); len(t) > 0 && t[0] == '{' {
		var settings struct {
			Config json.RawMessage `json:"config"`
		}
		if err := json.Unmarshal(g.Settings, &settings); err == nil {
			out.Hostname = jsonObjectString(settings.Config, "hostname")
		}
	}
	return out, nil
}

// ResolveServerName is what a connect flow calls for the service the owner picked: one gameserver
// read (best effort - a failure or a response for another service just leaves those fields out)
// and then ServerName. "" means Nitrado reported no name at all.
func (c *Client) ResolveServerName(ctx context.Context, svc Service) string {
	var gs GameserverName
	if c != nil && svc.ID != "" {
		if got, err := c.GameserverName(ctx, svc.ID); err == nil && got.BelongsTo(svc.ID) {
			gs = got
		}
	}
	return ServerName(gs, svc)
}
