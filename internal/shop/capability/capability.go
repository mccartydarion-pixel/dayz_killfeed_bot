// Package capability is Champion Shop Phase 2C.1 (docs/SHOP_DELIVERY_PHASE2C1.md): READ-ONLY
// discovery of whether a connected console DayZ / Nitrado service could, in a later phase, receive
// Champion's object-spawner file.
//
// It is incapable of writing: Reader - the only Nitrado surface it receives - has read methods
// only, there is no upload/copy/move/delete method anywhere in internal/nitrado, and every path it
// reads is validated to lie inside the service's own file-browser root. Secrets never enter it:
// the Nitrado facts are whitelist-decoded (nitrado.ServiceFacts / GameserverFacts / TokenFacts), a
// downloaded serverDZ.cfg is reduced to named non-secret keys, and evidence strings are built only
// from those values.
package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// Classification of a finding (the Phase 2B/2C vocabulary).
const (
	VerifiedLive  = "VERIFIED LIVE"
	VerifiedTest  = "VERIFIED IN TEST"
	Documentation = "SUPPORTED BY DOCUMENTATION"
	Unverified    = "UNVERIFIED"
	Unsupported   = "UNSUPPORTED"
)

// ChampionArtifact is the proposed Champion spawner file, relative to the mission folder
// (nitradodelivery.ArtifactRelPath). Discovery only looks for it; it never adds it anywhere.
const ChampionArtifact = "champion/champion_shop_delivery.json"

// Reader is every Nitrado call discovery may make. All are reads (GET / signed download).
type Reader interface {
	ServiceFacts(ctx context.Context, serviceID string) (nitrado.ServiceFacts, error)
	GameserverFacts(ctx context.Context, serviceID string) (nitrado.GameserverFacts, error)
	TokenFacts(ctx context.Context) (nitrado.TokenFacts, error)
	ListDir(ctx context.Context, serviceID, dir string) ([]nitrado.LogFile, error)
	ReadLog(ctx context.Context, serviceID, path string) ([]byte, error)
}

// Binding is the installation's stored Nitrado binding (installations.game_server_id ->
// game_servers.provider_service_id), scoped by organization.
type Binding struct {
	OrganizationID   int64
	InstallationID   int64
	GameServerID     int64
	NitradoServiceID string
}

// Finding is one classified result with sanitized evidence.
type Finding struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Class    string `json:"class"`
	Value    string `json:"value,omitempty"`
	Evidence string `json:"evidence"`
}

// Report is the discovery result.
type Report struct {
	ServiceID      string    `json:"serviceId"`
	FileRoot       string    `json:"fileRoot,omitempty"`
	MissionDir     string    `json:"missionDir,omitempty"`   // physical path used for reads
	MissionPath    string    `json:"missionPath,omitempty"`  // canonical, mount-independent
	SpawnerFiles   []string  `json:"spawnerFiles,omitempty"` // objectSpawnersArr entries
	Findings       []Finding `json:"findings"`
	ReadOperations int       `json:"readOperations"`
}

func (r *Report) add(key, label, class, value, evidence string) {
	r.Findings = append(r.Findings, Finding{Key: key, Label: label, Class: class, Value: value, Evidence: evidence})
}

// Get returns a finding by key.
func (r Report) Get(key string) (Finding, bool) {
	for _, f := range r.Findings {
		if f.Key == key {
			return f, true
		}
	}
	return Finding{}, false
}

var (
	serviceIDRe  = regexp.MustCompile(`^[0-9]{1,12}$`)
	missionRe    = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9_]+)?$`) // e.g. dayzOffline.chernarusplus
	fileRootRe   = regexp.MustCompile(`^/games/[A-Za-z0-9_]+$`)
	mountMarkers = []string{"/noftp/", "/ftproot/"}

	// ErrBinding: the requested service is not the installation's bound service.
	ErrBinding = errors.New("capability: service is not the installation's bound Nitrado service")
	// ErrUnsafePath: a path escapes the service's file-browser root.
	ErrUnsafePath = errors.New("capability: path outside the service file-browser root")
)

// FileRoot derives the file-browser root ("/games/<account>") from game_specific.path, or "".
func FileRoot(gamePath string) string {
	p := path.Clean("/" + strings.TrimSpace(gamePath))
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) < 2 || parts[0] != "games" {
		return ""
	}
	root := "/games/" + parts[1]
	if !fileRootRe.MatchString(root) {
		return ""
	}
	return root
}

// SafePath cleans p and requires it to lie inside root. It never returns a path with "..".
func SafePath(root, p string) (string, error) {
	if root == "" || !fileRootRe.MatchString(root) || strings.Contains(p, "\x00") || strings.Contains(p, "\\") {
		return "", ErrUnsafePath
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", ErrUnsafePath
		}
	}
	clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
	if clean != root && !strings.HasPrefix(clean, root+"/") {
		return "", ErrUnsafePath
	}
	return clean, nil
}

// Canonical strips the ftproot/noftp mount so both representations are one location.
func Canonical(p string) string {
	for _, m := range mountMarkers {
		if i := strings.Index(p, m); i >= 0 {
			return p[i+len(m):]
		}
	}
	return p
}

// ServerCfgFacts are the ONLY keys ever kept from a serverDZ.cfg. Passwords, hostname, admin and
// RCON values are never read into this struct.
type ServerCfgFacts struct {
	Present               bool
	EnableCfgGameplayFile string // "" = key absent
	MissionTemplate       string
}

var (
	cfgKeyRe      = regexp.MustCompile(`(?m)^\s*enableCfgGameplayFile\s*=\s*([0-9]+)\s*;`)
	cfgTemplateRe = regexp.MustCompile(`(?s)class\s+Missions\s*\{.*?template\s*=\s*"([A-Za-z0-9_.]+)"`)
)

// ParseServerCfg extracts the whitelisted keys from a serverDZ.cfg. It never returns any other text.
func ParseServerCfg(raw []byte) ServerCfgFacts {
	f := ServerCfgFacts{Present: len(raw) > 0}
	if m := cfgKeyRe.FindSubmatch(raw); m != nil {
		f.EnableCfgGameplayFile = string(m[1])
	}
	if m := cfgTemplateRe.FindSubmatch(raw); m != nil {
		f.MissionTemplate = string(m[1])
	}
	return f
}

// GameplayFacts are what cfggameplay.json says about object spawners.
type GameplayFacts struct {
	ValidJSON        bool
	Version          int
	HasWorldsData    bool
	HasSpawnersKey   bool
	Spawners         []string
	ReferencesChamp  bool
	InvalidSpawnPath []string // entries that are not a safe relative .json path
}

var spawnerPathRe = regexp.MustCompile(`^[A-Za-z0-9_\-]+(/[A-Za-z0-9_\-]+)*\.json$`)

// ParseCfgGameplay parses cfggameplay.json (not sensitive) for its spawner configuration.
func ParseCfgGameplay(raw []byte) GameplayFacts {
	var wire struct {
		Version    *int `json:"version"`
		WorldsData *struct {
			ObjectSpawnersArr *[]string `json:"objectSpawnersArr"`
		} `json:"WorldsData"`
	}
	f := GameplayFacts{}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return f
	}
	f.ValidJSON = true
	if wire.Version != nil {
		f.Version = *wire.Version
	}
	if wire.WorldsData != nil {
		f.HasWorldsData = true
		if wire.WorldsData.ObjectSpawnersArr != nil {
			f.HasSpawnersKey = true
			for _, s := range *wire.WorldsData.ObjectSpawnersArr {
				s = strings.TrimSpace(s)
				f.Spawners = append(f.Spawners, s)
				if s == ChampionArtifact {
					f.ReferencesChamp = true
				}
				if !spawnerPathRe.MatchString(s) {
					f.InvalidSpawnPath = append(f.InvalidSpawnPath, s)
				}
			}
		}
	}
	return f
}

func has(roles []string, want string) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}

// Discover runs the read-only discovery for one installation's bound service.
func Discover(ctx context.Context, rd Reader, b Binding) (Report, error) {
	rep := Report{ServiceID: b.NitradoServiceID}
	if b.OrganizationID <= 0 || b.InstallationID <= 0 || b.GameServerID <= 0 || !serviceIDRe.MatchString(b.NitradoServiceID) {
		return rep, ErrBinding
	}

	// 1. Service identity and permissions.
	svc, err := rd.ServiceFacts(ctx, b.NitradoServiceID)
	rep.ReadOperations++
	if err != nil {
		rep.add("service_identity", "Nitrado service identity", Unverified, "", "service details unavailable: "+errClass(err))
		return rep, nil
	}
	if fmt.Sprint(svc.ID) != b.NitradoServiceID {
		return rep, ErrBinding
	}
	rep.add("service_identity", "Nitrado service identity", VerifiedLive, fmt.Sprintf("%d", svc.ID),
		fmt.Sprintf("GET /services/%d: type=%s status=%s game=%s is_owner=%t readonly=%t", svc.ID, svc.Type, svc.Status, svc.Game, svc.IsOwner, svc.ReadOnly))
	readRole, writeRole := has(svc.Roles, "ROLE_WEBINTERFACE_FILEBROWSER_READ"), has(svc.Roles, "ROLE_WEBINTERFACE_FILEBROWSER_WRITE")
	rep.add("filebrowser_read_role", "File-browser READ permission", classIf(readRole, VerifiedLive, Unverified), fmt.Sprint(readRole),
		fmt.Sprintf("service roles (%d) include ROLE_WEBINTERFACE_FILEBROWSER_READ=%t", len(svc.Roles), readRole))
	// A role listed on the service is documentation-level evidence; only a real write proves it.
	rep.add("filebrowser_write_role", "File-browser WRITE permission", classIf(writeRole, Documentation, Unverified), fmt.Sprint(writeRole),
		fmt.Sprintf("service roles include ROLE_WEBINTERFACE_FILEBROWSER_WRITE=%t (the documented upload permission); no write was attempted, so write capability is not proven", writeRole))
	rep.add("service_readonly", "Service read-only flag", VerifiedLive, fmt.Sprint(svc.ReadOnly), fmt.Sprintf("service.readonly=%t", svc.ReadOnly))

	tok, err := rd.TokenFacts(ctx)
	rep.ReadOperations++
	if err != nil {
		rep.add("token_scopes", "Token scopes", Unverified, "", "token details unavailable: "+errClass(err))
	} else {
		sc := append([]string(nil), tok.Scopes...)
		sort.Strings(sc)
		rep.add("token_scopes", "Token scopes", VerifiedLive, strings.Join(sc, ","), "GET /token scopes (the token itself is never read)")
	}

	// 2. Gameserver type, platform, file browser and settings.
	gs, err := rd.GameserverFacts(ctx, b.NitradoServiceID)
	rep.ReadOperations++
	if err != nil {
		rep.add("gameserver", "Gameserver details", Unverified, "", "gameserver details unavailable: "+errClass(err))
		return rep, nil
	}
	if gs.ServiceID != 0 && fmt.Sprint(gs.ServiceID) != b.NitradoServiceID {
		return rep, ErrBinding
	}
	platform := "unknown"
	switch gs.Game {
	case "dayzps":
		platform = "PlayStation"
	case "dayzxb":
		platform = "Xbox"
	case "dayzstandalone", "dayz":
		platform = "PC"
	}
	rep.add("gameserver_platform", "Game and platform", VerifiedLive, gs.Game+" / "+platform,
		fmt.Sprintf("gameserver.game=%s (%s) status=%s slots=%d map=%s version=%s", gs.Game, gs.GameHuman, gs.Status, gs.Slots, gs.QueryMap, gs.QueryVersion))
	rep.add("file_browser", "File-browser feature", classIf(gs.HasFileBrowser, VerifiedLive, Unsupported), fmt.Sprint(gs.HasFileBrowser),
		fmt.Sprintf("features.has_file_browser=%t has_ftp=%t has_backups=%t has_expert_mode=%t", gs.HasFileBrowser, gs.HasFTP, gs.HasBackups, gs.HasExpertMode))
	rep.add("setting_enableCfgGameplayFile", "enableCfgGameplayFile (Nitrado setting)", classIf(gs.EnableCfgGameplayFile != "", VerifiedLive, Unverified), gs.EnableCfgGameplayFile,
		"settings.config.enableCfgGameplayFile (the serverDZ.cfg value Nitrado manages)")
	rep.add("setting_mission", "Mission (Nitrado setting)", classIf(gs.Mission != "", VerifiedLive, Unverified), gs.Mission, "settings.config.mission")

	root := FileRoot(gs.GamePath)
	if root == "" {
		rep.add("file_root", "File-browser root", Unverified, "", "game_specific.path does not have the /games/<account>/... form")
		return rep, nil
	}
	rep.FileRoot = root
	rep.add("file_root", "File-browser root", VerifiedLive, "/games/<account>", "derived from game_specific.path; every read below is validated inside it")

	// 2b. serverDZ.cfg, if the file browser exposes one (console services may manage it only
	// through the settings above). Only whitelisted keys are kept; the file text is discarded.
	rep.add("serverdz_cfg", "serverDZ.cfg", Unverified, "not listed", "no serverDZ*.cfg listed in the game directory")
	if gameDir, err := SafePath(root, gs.GamePath); err == nil {
		files, lerr := rd.ListDir(ctx, b.NitradoServiceID, gameDir)
		rep.ReadOperations++
		for _, f := range files {
			if lerr != nil || !serverCfgNameRe.MatchString(f.Name) {
				continue
			}
			p, perr := SafePath(root, f.Path)
			if perr != nil {
				continue
			}
			raw, rerr := rd.ReadLog(ctx, b.NitradoServiceID, p)
			rep.ReadOperations++
			if rerr != nil {
				rep.setLast("serverdz_cfg", Unverified, "listed", f.Name+" listed; download failed: "+errClass(rerr))
				break
			}
			cfg := ParseServerCfg(raw)
			match := cfg.EnableCfgGameplayFile == "" || gs.EnableCfgGameplayFile == "" || cfg.EnableCfgGameplayFile == gs.EnableCfgGameplayFile
			rep.setLast("serverdz_cfg", VerifiedLive, "present",
				fmt.Sprintf("%s: enableCfgGameplayFile=%q template=%q (matches Nitrado setting: %t; no other key is read)", f.Name, cfg.EnableCfgGameplayFile, cfg.MissionTemplate, match))
			break
		}
	}

	// 3. Mission directory: the dayzps_missions folder of a mount, containing the configured mission.
	if !missionRe.MatchString(gs.Mission) {
		rep.add("mission_dir", "Mission folder", Unverified, "", "the mission setting is not a plain mission name")
		return rep, nil
	}
	var missionDir string
	var missionFiles []nitrado.LogFile
	for _, mount := range []string{"ftproot", "noftp"} {
		missionsRoot, err := SafePath(root, root+"/"+mount+"/"+gs.Game+"_missions")
		if err != nil {
			continue
		}
		cand, _ := SafePath(root, missionsRoot+"/"+gs.Mission)
		files, err := rd.ListDir(ctx, b.NitradoServiceID, cand)
		rep.ReadOperations++
		if err == nil && len(files) > 0 {
			missionDir, missionFiles = cand, files
			break
		}
	}
	if missionDir == "" {
		rep.add("mission_dir", "Mission folder", Unverified, "", "no listed mission folder under <root>/{ftproot,noftp}/"+gs.Game+"_missions/"+gs.Mission)
		return rep, nil
	}
	rep.MissionDir = missionDir
	rep.MissionPath = Canonical(missionDir)
	rep.add("mission_dir", "Mission folder", VerifiedLive, rep.MissionPath, fmt.Sprintf("listed %d entries in %s (canonical, mount-independent)", len(missionFiles), rep.MissionPath))

	// 4. cfggameplay.json in the mission folder.
	var gameplayPath string
	for _, f := range missionFiles {
		if strings.EqualFold(f.Name, "cfggameplay.json") {
			gameplayPath = f.Path
		}
	}
	if gameplayPath == "" {
		rep.add("cfggameplay", "cfggameplay.json", Unverified, "absent", "not listed in the mission folder")
		return rep, nil
	}
	gameplayPath, err = SafePath(root, gameplayPath)
	if err != nil {
		rep.add("cfggameplay", "cfggameplay.json", Unverified, "", "listed path outside the file-browser root")
		return rep, nil
	}
	raw, err := rd.ReadLog(ctx, b.NitradoServiceID, gameplayPath)
	rep.ReadOperations++
	if err != nil {
		rep.add("cfggameplay", "cfggameplay.json", Unverified, "listed", "download failed: "+errClass(err))
		return rep, nil
	}
	gp := ParseCfgGameplay(raw)
	if !gp.ValidJSON {
		rep.add("cfggameplay", "cfggameplay.json", VerifiedLive, "present, INVALID JSON", fmt.Sprintf("%d bytes; not parseable - DayZ would not load it", len(raw)))
		return rep, nil
	}
	rep.add("cfggameplay", "cfggameplay.json", VerifiedLive, "present", fmt.Sprintf("%d bytes, version=%d, sha256=%s", len(raw), gp.Version, shortSHA(raw)))
	rep.SpawnerFiles = gp.Spawners
	rep.add("objectSpawnersArr", "WorldsData.objectSpawnersArr", classIf(gp.HasSpawnersKey, VerifiedLive, Unverified), fmt.Sprint(len(gp.Spawners)),
		fmt.Sprintf("key present=%t entries=%d invalid entries=%d", gp.HasSpawnersKey, len(gp.Spawners), len(gp.InvalidSpawnPath)))
	rep.add("champion_referenced", "Champion artifact referenced", VerifiedLive, fmt.Sprint(gp.ReferencesChamp),
		fmt.Sprintf("objectSpawnersArr contains %q: %t (discovery never adds it)", ChampionArtifact, gp.ReferencesChamp))

	// 5. Referenced spawner files exist? (listing their folders only; contents are not needed)
	listed := map[string]bool{}
	for _, f := range missionFiles {
		listed[f.Name] = true
	}
	dirCache := map[string]map[string]bool{"": listed}
	missing := []string{}
	champExists := false
	for _, s := range append(append([]string(nil), gp.Spawners...), ChampionArtifact) {
		if !spawnerPathRe.MatchString(s) {
			continue
		}
		dir, name := path.Split(s)
		dir = strings.TrimSuffix(dir, "/")
		names, ok := dirCache[dir]
		if !ok {
			names = map[string]bool{}
			if p, err := SafePath(root, missionDir+"/"+dir); err == nil {
				if files, err := rd.ListDir(ctx, b.NitradoServiceID, p); err == nil {
					for _, f := range files {
						names[f.Name] = true
					}
				}
				rep.ReadOperations++
			}
			dirCache[dir] = names
		}
		if s == ChampionArtifact {
			champExists = names[name]
			continue
		}
		if !names[name] {
			missing = append(missing, s)
		}
	}
	rep.add("spawner_files_exist", "Referenced spawner files exist", VerifiedLive, fmt.Sprintf("%d/%d", len(gp.Spawners)-len(missing), len(gp.Spawners)),
		fmt.Sprintf("missing: %s", strings.Join(missing, ", ")))
	rep.add("champion_artifact_exists", "Champion artifact file exists", VerifiedLive, fmt.Sprint(champExists), fmt.Sprintf("%s listed in the mission folder: %t", ChampionArtifact, champExists))
	return rep, nil
}

func classIf(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}

// errClass reduces an error to a safe label (never a URL, token or path).
func errClass(err error) string {
	var re *nitrado.RequestError
	if errors.As(err, &re) {
		return fmt.Sprintf("nitrado_%s_%d", re.Kind, re.StatusCode)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "error"
}

func shortSHA(b []byte) string {
	sum := sha256sum(b)
	return sum[:12] + "…"
}

var serverCfgNameRe = regexp.MustCompile(`(?i)^serverDZ[A-Za-z0-9_]*\.cfg$`)

// setLast replaces the class, value and evidence of an existing finding.
func (r *Report) setLast(key, class, value, evidence string) {
	for i := range r.Findings {
		if r.Findings[i].Key == key {
			r.Findings[i].Class, r.Findings[i].Value, r.Findings[i].Evidence = class, value, evidence
			return
		}
	}
	r.add(key, key, class, value, evidence)
}
