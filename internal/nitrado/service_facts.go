package nitrado

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Read-only, whitelist-decoded service facts (Shop Phase 2C.1 capability discovery,
// docs/SHOP_DELIVERY_PHASE2C1.md).
//
// GET /services/:id/gameservers returns FTP and MySQL credentials, the admin and RCON passwords, the
// server password and a websocket token in the same body. These methods decode ONLY the named safe
// fields below into typed structs: a secret is never unmarshalled into memory the caller can see,
// log or return, and the raw body is never kept. Every method is a GET.

// ServiceFacts are the non-sensitive facts of GET /services/:id.
type ServiceFacts struct {
	ID       int64    `json:"id"`
	Type     string   `json:"type"`
	SubType  string   `json:"sub_type"`
	Status   string   `json:"status"`
	IsOwner  bool     `json:"is_owner"`
	ReadOnly bool     `json:"readonly"`
	Roles    []string `json:"roles"` // role names only (e.g. ROLE_WEBINTERFACE_FILEBROWSER_READ)
	Game     string   `json:"-"`
}

// GameserverFacts are the non-sensitive facts of GET /services/:id/gameservers.
type GameserverFacts struct {
	ServiceID                                                  int64
	Game                                                       string // e.g. "dayzps"
	GameHuman                                                  string
	Status                                                     string
	Type                                                       string
	Slots                                                      int
	MustBeStarted                                              bool
	GamePath                                                   string // game_specific.path: the game directory inside the file browser
	PathAvailable                                              bool
	HasFileBrowser, HasFTP, HasBackups, HasExpertMode, HasRCON bool
	LogFileCount                                               int
	QueryMap                                                   string
	QueryVersion                                               string
	// Whitelisted DayZ settings (serverDZ.cfg as Nitrado manages it). Never password, admin-password,
	// rcon-password or hostname.
	EnableCfgGameplayFile string
	Mission               string
	ExpertMode            string
	AdminLogPlayerList    string
}

// TokenFacts are the non-sensitive facts of GET /token: the scopes only.
type TokenFacts struct {
	Scopes []string
}

func (c *Client) getJSON(ctx context.Context, endpoint, op string, into any) error {
	resp, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyStatus(op, resp.StatusCode, KindNotFound)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read %s: %w", op, err)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("decode %s: %w", op, err)
	}
	return nil
}

// ServiceFacts reads GET /services/:id (whitelisted fields only).
func (c *Client) ServiceFacts(ctx context.Context, serviceID string) (ServiceFacts, error) {
	var wire struct {
		Data struct {
			Service struct {
				ServiceFacts
				Details struct {
					Game string `json:"game"`
				} `json:"details"`
			} `json:"service"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/services/"+url.PathEscape(serviceID), "service details", &wire); err != nil {
		return ServiceFacts{}, err
	}
	f := wire.Data.Service.ServiceFacts
	f.Game = wire.Data.Service.Details.Game
	return f, nil
}

// GameserverFacts reads GET /services/:id/gameservers (whitelisted fields only).
func (c *Client) GameserverFacts(ctx context.Context, serviceID string) (GameserverFacts, error) {
	var wire struct {
		Data struct {
			Gameserver struct {
				ServiceID     int64  `json:"service_id"`
				Game          string `json:"game"`
				GameHuman     string `json:"game_human"`
				Status        string `json:"status"`
				Type          string `json:"type"`
				Slots         int    `json:"slots"`
				MustBeStarted bool   `json:"must_be_started"`
				GameSpecific  struct {
					Path          string   `json:"path"`
					PathAvailable bool     `json:"path_available"`
					LogFiles      []string `json:"log_files"`
					Features      struct {
						HasFileBrowser bool `json:"has_file_browser"`
						HasFTP         bool `json:"has_ftp"`
						HasBackups     bool `json:"has_backups"`
						HasExpertMode  bool `json:"has_expert_mode"`
						HasRCON        bool `json:"has_rcon"`
					} `json:"features"`
				} `json:"game_specific"`
				Query struct {
					Map     string `json:"map"`
					Version string `json:"version"`
				} `json:"query"`
				Settings struct {
					Config struct {
						EnableCfgGameplayFile string `json:"enableCfgGameplayFile"`
						Mission               string `json:"mission"`
						AdminLogPlayerList    string `json:"adminLogPlayerList"`
					} `json:"config"`
					General struct {
						ExpertMode string `json:"expertMode"`
					} `json:"general"`
				} `json:"settings"`
			} `json:"gameserver"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/services/"+url.PathEscape(serviceID)+"/gameservers", "gameserver details", &wire); err != nil {
		return GameserverFacts{}, err
	}
	g := wire.Data.Gameserver
	return GameserverFacts{
		ServiceID: g.ServiceID, Game: g.Game, GameHuman: g.GameHuman, Status: g.Status, Type: g.Type, Slots: g.Slots, MustBeStarted: g.MustBeStarted,
		GamePath: g.GameSpecific.Path, PathAvailable: g.GameSpecific.PathAvailable,
		HasFileBrowser: g.GameSpecific.Features.HasFileBrowser, HasFTP: g.GameSpecific.Features.HasFTP, HasBackups: g.GameSpecific.Features.HasBackups,
		HasExpertMode: g.GameSpecific.Features.HasExpertMode, HasRCON: g.GameSpecific.Features.HasRCON,
		LogFileCount: len(g.GameSpecific.LogFiles), QueryMap: g.Query.Map, QueryVersion: g.Query.Version,
		EnableCfgGameplayFile: g.Settings.Config.EnableCfgGameplayFile, Mission: g.Settings.Config.Mission,
		ExpertMode: g.Settings.General.ExpertMode, AdminLogPlayerList: g.Settings.Config.AdminLogPlayerList,
	}, nil
}

// TokenFacts reads GET /token (scopes only; never the token, its id or its owner).
func (c *Client) TokenFacts(ctx context.Context) (TokenFacts, error) {
	var wire struct {
		Data struct {
			Token struct {
				Scopes []string `json:"scopes"`
			} `json:"token"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/token", "token details", &wire); err != nil {
		return TokenFacts{}, err
	}
	return TokenFacts{Scopes: wire.Data.Token.Scopes}, nil
}

// GameserverLive is the live occupancy Nitrado reports for a gameserver: its run
// status and the game query's current/max player counts (GET
// /services/:id/gameservers, whitelisted fields only - the same body carries
// credentials, which are never decoded).
//
// PlayerCurrent is nil when Nitrado has no query result (server starting,
// stopped, query not yet run, or the query block serialized as an empty
// array). A nil PlayerCurrent is "unknown", never zero.
type GameserverLive struct {
	Status        string // e.g. "started", "stopped", "restarting"
	Slots         int
	PlayerCurrent *int
	PlayerMax     int
}

// Running reports whether Nitrado says the game process is up.
func (g GameserverLive) Running() bool { return g.Status == "started" }

// Stopped reports whether Nitrado says the game process is definitely down, so
// no player can be connected.
func (g GameserverLive) Stopped() bool { return g.Status == "stopped" || g.Status == "suspended" }

// Capacity is the player slot count to display: the query's max when known,
// else the service's slot count.
func (g GameserverLive) Capacity() int {
	if g.PlayerMax > 0 {
		return g.PlayerMax
	}
	return g.Slots
}

// GameserverLive reads the live status and query player counts for a service.
func (c *Client) GameserverLive(ctx context.Context, serviceID string) (GameserverLive, error) {
	var wire struct {
		Data struct {
			Gameserver struct {
				Status string          `json:"status"`
				Slots  int             `json:"slots"`
				Query  json.RawMessage `json:"query"`
			} `json:"gameserver"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/services/"+url.PathEscape(serviceID)+"/gameservers", "gameserver status", &wire); err != nil {
		return GameserverLive{}, err
	}
	g := wire.Data.Gameserver
	out := GameserverLive{Status: g.Status, Slots: g.Slots}
	// Nitrado serializes an empty query as [] rather than {}; only an object
	// carries counts.
	if q := bytes.TrimSpace(g.Query); len(q) > 0 && q[0] == '{' {
		var query struct {
			PlayerCurrent *int `json:"player_current"`
			PlayerMax     int  `json:"player_max"`
		}
		if err := json.Unmarshal(q, &query); err == nil {
			if query.PlayerCurrent != nil && *query.PlayerCurrent >= 0 {
				out.PlayerCurrent = query.PlayerCurrent
			}
			out.PlayerMax = query.PlayerMax
		}
	}
	return out, nil
}
