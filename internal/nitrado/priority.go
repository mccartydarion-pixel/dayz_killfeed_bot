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

// The priority list ("Prioritized players" in Nitrado's web panel): players who skip the queue
// when the server is full.
//
// Unlike the whitelist and ban list, Nitrado has no add/remove endpoint for it. It is one setting,
// settings.general.priority, holding every name on its own line, and it is changed by writing the
// whole value back:
//
//	GET  /services/{id}/gameservers           -> data.gameserver.settings.general.priority
//	POST /services/{id}/gameservers/settings  body: {"category":"general","key":"priority","value":"..."}
//
// The write replaces the whole list, so callers must read, change and write (see PriorityList).
// Neither call has been exercised against a live Nitrado service from this codebase. The read
// guards the write: PriorityList reports ErrPriorityUnsupported when the service does not carry
// the setting at all, and nothing is ever written to a service whose list could not be read first.

// ErrPriorityUnsupported means the service's settings carry no priority list.
var ErrPriorityUnsupported = errors.New("nitrado: this service has no priority list setting")

// MaxPriorityEntries bounds the list Champion will write.
const MaxPriorityEntries = 500

// ParsePriorityList splits Nitrado's one-name-per-line value into names: trimmed, blanks dropped,
// duplicates (ignoring case) removed, order kept.
func ParsePriorityList(value string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, line := range strings.FieldsFunc(value, func(r rune) bool { return r == '\r' || r == '\n' }) {
		name := strings.TrimSpace(line)
		key := strings.ToLower(name)
		if name == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out
}

// ValidPriorityName reports whether name can sit on one line of the list: non-empty, at most 64
// characters, no line breaks or other control characters.
func ValidPriorityName(name string) bool {
	if name == "" || len(name) > 64 || name != strings.TrimSpace(name) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// PriorityList reads the service's priority list. Only that one setting is decoded: the same body
// carries passwords, which never reach memory the caller can see.
func (c *Client) PriorityList(ctx context.Context, serviceID string) ([]string, error) {
	if serviceID == "" {
		return nil, fmt.Errorf("service ID is required")
	}
	var wire struct {
		Data struct {
			Gameserver struct {
				Settings struct {
					General struct {
						Priority *string `json:"priority"`
					} `json:"general"`
				} `json:"settings"`
			} `json:"gameserver"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/services/"+url.PathEscape(serviceID)+"/gameservers", "gameserver priority list", &wire); err != nil {
		return nil, err
	}
	value := wire.Data.Gameserver.Settings.General.Priority
	if value == nil {
		return nil, ErrPriorityUnsupported
	}
	return ParsePriorityList(*value), nil
}

// SetPriorityList replaces the service's priority list with names. Call it only with a list that
// came from PriorityList on the same service a moment ago, changed by one entry.
func (c *Client) SetPriorityList(ctx context.Context, serviceID string, names []string) error {
	if serviceID == "" {
		return fmt.Errorf("service ID is required")
	}
	if len(names) > MaxPriorityEntries {
		return fmt.Errorf("priority list is limited to %d players", MaxPriorityEntries)
	}
	for _, name := range names {
		if !ValidPriorityName(name) {
			return fmt.Errorf("priority list holds a name that cannot be written")
		}
	}
	body, err := json.Marshal(map[string]string{"category": "general", "key": "priority", "value": strings.Join(names, "\r\n")})
	if err != nil {
		return fmt.Errorf("encode priority list: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/services/"+url.PathEscape(serviceID)+"/gameservers/settings", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyStatus("gameserver_priority", resp.StatusCode, KindNotFound)
	}
	return nil
}
