package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/featureflags"
	"github.com/yourname/dayz-killfeed/internal/maprotation"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Map rotation with a player vote (docs/MAP_ROTATION.md): the owner's settings and the player's
// vote. Nothing in this file writes to a game server; the switch itself is in
// map_rotation_worker.go.
//
// The feature is off at three levels, and every one must be on before anything happens: the
// map_rotation feature flag (default off), the plan (entitlements.MapRotation) and the owner's own
// `enabled` setting.

const (
	codeValidationError = "VALIDATION_ERROR"
	codeNotLinked       = "NOT_LINKED"
	codeVoteClosed      = "VOTE_CLOSED"
)

func init() {
	httpStatusForCode[codeValidationError] = http.StatusBadRequest
	httpStatusForCode[codeNotLinked] = http.StatusForbidden
	httpStatusForCode[codeVoteClosed] = http.StatusConflict
}

// mapRotationRemote is the Nitrado surface map rotation uses: the reads the file check needs, the
// scheduled tasks (for the next restart), the two write primitives, which only
// internal/maprotation/mapswitch ever calls, and what clearing the saved characters needs (stop,
// restart and the delete of players.db), which only internal/maprotation/charwipe ever calls.
// *nitrado.Client satisfies it.
type mapRotationRemote interface {
	maprotation.Reader
	ListScheduledTasks(ctx context.Context, serviceID string) ([]nitrado.ScheduledTask, error)
	RequestUploadToken(ctx context.Context, serviceID, dir, name string) (nitrado.UploadTarget, error)
	PostUpload(ctx context.Context, t nitrado.UploadTarget, data []byte) error
	Stop(ctx context.Context, serviceID, message string) error
	Restart(ctx context.Context, serviceID, message string) error
	DeleteFile(ctx context.Context, serviceID, path string) error
}

func (a *App) registerMapRotationRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/map/rotation", a.handleAdminMapRotation)
	h("PUT "+adminBase+"/map/rotation", a.handleSaveMapRotation)
	h("POST "+adminBase+"/map/rotation/check", a.handleCheckMapRotation)
	h("POST "+adminBase+"/map/rotation/next", a.handleSetNextMap)
	h("GET /api/saas/player/servers/{installationID}/map/vote", a.handlePlayerMapVote)
	h("POST /api/saas/player/servers/{installationID}/map/vote", a.handleCastMapVote)
}

// mapRotationRemote opens the installation's Nitrado access with the organization's stored
// credential. The token never leaves the client.
func (a *App) mapRotationRemote(ctx context.Context, t repository.MapRotationTarget) (mapRotationRemote, error) {
	if a.mapRotationRemoteFor != nil {
		return a.mapRotationRemoteFor(ctx, t)
	}
	if t.NitradoServiceID == "" || a.SaaSCredentials == nil {
		return nil, errors.New("no Nitrado service is connected")
	}
	envelope, err := a.SaaSCredentials.GetForOrganizationOnly(ctx, t.OrganizationID)
	if err != nil || envelope == nil {
		return nil, errors.New("no Nitrado credential is connected")
	}
	return a.nitradoClientFromEnvelope(*envelope)
}

// --- the three switches ----------------------------------------------------------------------------

// mapRotationFlag is the feature flag for one installation: the platform owner's override when
// there is one, else CHAMPION_MAP_ROTATION_ENABLED (default off).
func (a *App) mapRotationFlag(installationID int64) bool {
	def := a.Config != nil && a.Config.MapRotationEnabled
	if a.FeatureFlags == nil {
		return def
	}
	return a.FeatureFlags.Enabled(installationID, featureflags.MapRotation, def)
}

const (
	mapRotationReasonFlag = "Map rotation is not switched on for this server yet."
	mapRotationReasonPlan = "Map rotation is part of the Champion plan. Upgrade to use it."
)

// mapRotationAvailable answers whether the installation may use map rotation at all: the feature
// flag and the plan. reason is empty when it may. planBlocked tells a write which error to send.
func (a *App) mapRotationAvailable(ctx context.Context, organizationID, installationID int64) (reason string, planBlocked bool, err error) {
	if !a.mapRotationFlag(installationID) {
		return mapRotationReasonFlag, false, nil
	}
	if entitlements.Enforced() {
		plan, err := a.organizationPlan(ctx, organizationID)
		if err != nil {
			return "", false, err
		}
		if !entitlements.Has(plan, entitlements.MapRotation) {
			return mapRotationReasonPlan, true, nil
		}
	}
	return "", false, nil
}

// --- DTOs (the contract in docs/MAP_ROTATION.md) -----------------------------------------------------

// mapEntryDTO is one map. spawnFile is the name of the spawn file the owner uploaded; its contents
// are stored in Champion and are never returned, only whether they are there and how large.
type mapEntryDTO struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	MapFile       string  `json:"mapFile"`
	SpawnFile     string  `json:"spawnFile"`
	SpawnUploaded bool    `json:"spawnUploaded"`
	SpawnBytes    int     `json:"spawnBytes"`
	ImageURL      *string `json:"imageUrl"`
	// The uploaded picture: whether one is stored, how large, and its version (null when none).
	// The bytes are never returned here; the public map-images route serves them.
	ImageUploaded bool    `json:"imageUploaded"`
	ImageBytes    int     `json:"imageBytes"`
	ImageVersion  *string `json:"imageVersion"`
	Enabled       bool    `json:"enabled"`
	Position      int     `json:"position"`
}

type mapVoteOptionDTO struct {
	MapID    int64   `json:"mapId"`
	Name     string  `json:"name"`
	ImageURL *string `json:"imageUrl"`
	// ImageVersion is non-null only while a picture is stored for the map (read when the view is
	// built, so a picture uploaded during a vote shows at once).
	ImageVersion *string `json:"imageVersion"`
	Votes        int     `json:"votes"`
}

type mapVoteDTO struct {
	ID         int64              `json:"id"`
	Status     string             `json:"status"`
	OpensAt    string             `json:"opensAt"`
	ClosesAt   string             `json:"closesAt"`
	Options    []mapVoteOptionDTO `json:"options"`
	TotalVotes int                `json:"totalVotes"`
}

type mapCurrentDTO struct {
	MapID *int64  `json:"mapId"`
	Name  *string `json:"name"`
	Since *string `json:"since"`
}

type mapNextDTO struct {
	MapID               *int64  `json:"mapId"`
	Name                *string `json:"name"`
	DecidedBy           *string `json:"decidedBy"`
	SwitchAt            *string `json:"switchAt"`
	RestartsUntilSwitch *int    `json:"restartsUntilSwitch"`
}

// mapLastSwitchDTO is the newest finished switch. charactersCleared is null when clearing the
// saved characters was not attempted (the option was off, or the switch did not succeed), else
// whether they were cleared; the message says why not.
type mapLastSwitchDTO struct {
	At                string  `json:"at"`
	MapID             *int64  `json:"mapId"`
	Name              *string `json:"name"`
	OK                bool    `json:"ok"`
	Message           string  `json:"message"`
	CharactersCleared *bool   `json:"charactersCleared"`
}

type mapFilesCheckDTO struct {
	MapID          int64  `json:"mapId"`
	MapFileFound   bool   `json:"mapFileFound"`
	SpawnFileFound bool   `json:"spawnFileFound"`
	CheckedAt      string `json:"checkedAt"`
}

type mapRotationAdminDTO struct {
	Available                bool               `json:"available"`
	Reason                   *string            `json:"reason"`
	Enabled                  bool               `json:"enabled"`
	EveryRestarts            int                `json:"everyRestarts"`
	Order                    string             `json:"order"`
	VoteEnabled              bool               `json:"voteEnabled"`
	VoteMinutesBeforeRestart int                `json:"voteMinutesBeforeRestart"`
	PingEveryone             bool               `json:"pingEveryone"`
	AnnounceChannelID        *string            `json:"announceChannelId"`
	WipeCharacters           bool               `json:"wipeCharacters"`
	Maps                     []mapEntryDTO      `json:"maps"`
	Current                  mapCurrentDTO      `json:"current"`
	Next                     mapNextDTO         `json:"next"`
	Vote                     *mapVoteDTO        `json:"vote"`
	LastSwitch               *mapLastSwitchDTO  `json:"lastSwitch"`
	FilesCheck               []mapFilesCheckDTO `json:"filesCheck"`
}

type mapPlayerCurrentDTO struct {
	MapID int64  `json:"mapId"`
	Name  string `json:"name"`
}

type mapPlayerNextDTO struct {
	MapID     int64  `json:"mapId"`
	Name      string `json:"name"`
	DecidedBy string `json:"decidedBy"`
}

type mapPlayerVoteDTO struct {
	mapVoteDTO
	MyVote *int64 `json:"myVote"`
}

type mapRotationPlayerDTO struct {
	Enabled    bool                 `json:"enabled"`
	Linked     bool                 `json:"linked"`
	ServerName string               `json:"serverName"`
	Current    *mapPlayerCurrentDTO `json:"current"`
	Next       *mapPlayerNextDTO    `json:"next"`
	Vote       *mapPlayerVoteDTO    `json:"vote"`
}

func toMapVoteDTO(v *repository.MapRotationVote) *mapVoteDTO {
	if v == nil {
		return nil
	}
	out := &mapVoteDTO{ID: v.ID, Status: v.Status, OpensAt: rfc3339(v.OpensAt), ClosesAt: rfc3339(v.ClosesAt), Options: make([]mapVoteOptionDTO, 0, len(v.Options)), TotalVotes: v.TotalVotes}
	for _, o := range v.Options {
		out.Options = append(out.Options, mapVoteOptionDTO{MapID: o.MapID, Name: o.Name, ImageURL: o.ImageURL, ImageVersion: o.ImageVersion, Votes: o.Votes})
	}
	return out
}

func rotationMaps(maps []repository.MapRotationMap) []maprotation.Map {
	out := make([]maprotation.Map, 0, len(maps))
	for _, m := range maps {
		out = append(out, maprotation.Map{ID: m.ID, Name: m.Name, MapFile: m.MapFile, SpawnFile: m.SpawnFile, Enabled: m.Enabled, Position: m.Position})
	}
	return out
}

func int64Value(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// mapRotationNext is the map the next switch loads as far as it is known now, and who decided it.
// A staff choice shows as soon as it is made; a sequence without a vote is known in advance; a
// vote or a random order is only known once decided.
func mapRotationNext(s repository.MapRotationSettings, maps []maprotation.Map) (maprotation.Map, string, bool) {
	decided, hasDecided := maprotation.Find(maps, int64Value(s.NextMapID))
	hasDecided = hasDecided && s.NextDecidedBy != nil
	if hasDecided && s.Phase == maprotation.PhaseDone {
		return decided, *s.NextDecidedBy, true
	}
	if m, ok := maprotation.Find(maps, int64Value(s.StaffNextMapID)); ok && m.Enabled {
		return m, maprotation.DecidedStaff, true
	}
	if hasDecided {
		return decided, *s.NextDecidedBy, true
	}
	if s.Enabled && !s.VoteEnabled && s.Order == maprotation.OrderSequence && len(maprotation.Enabled(maps)) >= maprotation.MinEnabledMaps {
		if m, ok := maprotation.NextInSequence(maps, int64Value(s.CurrentMapID)); ok {
			return m, maprotation.DecidedRotation, true
		}
	}
	return maprotation.Map{}, "", false
}

func toMapRotationAdminDTO(snap repository.MapRotationSnapshot, reason string, now time.Time) mapRotationAdminDTO {
	s := snap.Settings
	out := mapRotationAdminDTO{
		Available: reason == "", Enabled: s.Enabled, EveryRestarts: s.EveryRestarts, Order: s.Order, VoteEnabled: s.VoteEnabled,
		VoteMinutesBeforeRestart: s.VoteMinutes, PingEveryone: s.PingEveryone, AnnounceChannelID: s.AnnounceChannelID, WipeCharacters: s.WipeCharacters,
		Maps: make([]mapEntryDTO, 0, len(snap.Maps)), FilesCheck: []mapFilesCheckDTO{}, Vote: toMapVoteDTO(snap.Vote),
	}
	if reason != "" {
		out.Reason = &reason
	}
	for _, m := range snap.Maps {
		out.Maps = append(out.Maps, mapEntryDTO{ID: m.ID, Name: m.Name, MapFile: m.MapFile, SpawnFile: m.SpawnFile, SpawnUploaded: m.SpawnBytes > 0, SpawnBytes: m.SpawnBytes,
			ImageURL: m.ImageURL, ImageUploaded: m.ImageBytes > 0, ImageBytes: m.ImageBytes, ImageVersion: m.ImageVersion, Enabled: m.Enabled, Position: m.Position})
		// spawnFileFound: the spawn contents are stored in Champion (as they are now, not as they
		// were at the check).
		if m.CheckedAt != nil && m.MapFileFound != nil {
			out.FilesCheck = append(out.FilesCheck, mapFilesCheckDTO{MapID: m.ID, MapFileFound: *m.MapFileFound, SpawnFileFound: m.SpawnBytes > 0, CheckedAt: rfc3339(*m.CheckedAt)})
		}
	}
	if s.CurrentMapName != nil {
		out.Current = mapCurrentDTO{MapID: s.CurrentMapID, Name: s.CurrentMapName}
		if s.CurrentSince != nil {
			since := rfc3339(*s.CurrentSince)
			out.Current.Since = &since
		}
	}
	maps := rotationMaps(snap.Maps)
	if m, by, ok := mapRotationNext(s, maps); ok {
		out.Next.MapID, out.Next.Name, out.Next.DecidedBy = &m.ID, &m.Name, &by
	}
	if s.Enabled {
		n := maprotation.RestartsUntilSwitch(s.EveryRestarts, s.RestartsSinceSwitch)
		out.Next.RestartsUntilSwitch = &n
		if n == 1 && s.NextRestartAt != nil && s.NextRestartAt.After(now) {
			at := rfc3339(*s.NextRestartAt)
			out.Next.SwitchAt = &at
		}
	}
	if sw := snap.LastSwitch; sw != nil {
		at := sw.CreatedAt
		if sw.FinishedAt != nil {
			at = *sw.FinishedAt
		}
		name := sw.MapName
		out.LastSwitch = &mapLastSwitchDTO{At: rfc3339(at), MapID: sw.MapID, Name: &name, OK: sw.Status == repository.MapSwitchApplied, Message: sw.Message,
			CharactersCleared: sw.CharactersCleared}
	}
	// A stop of the rotation is said with the switch that caused it: a failed switch, or a switch
	// after which the server could not be started again (the rotation's stop reason is then part of
	// the switch's message).
	if s.HaltedReason != "" && out.LastSwitch != nil && (!out.LastSwitch.OK || strings.Contains(out.LastSwitch.Message, s.HaltedReason)) && !strings.Contains(out.LastSwitch.Message, "rotation is stopped") {
		out.LastSwitch.Message += " The rotation is stopped until you save the settings again."
	}
	return out
}

// --- admin -------------------------------------------------------------------------------------------

// mapRotationAdmin runs the owner-only preamble. For a write it also refuses when the feature is
// not available to the installation (the plan error when the plan is the reason).
func (a *App) mapRotationAdmin(w http.ResponseWriter, r *http.Request, write bool) (adminActor, string, bool) {
	ac, ok := a.requireCapability(w, r, permissions.CapMapRotationManage)
	if !ok {
		return ac, "", false
	}
	if a.MapRotation == nil {
		writeSaaSError(w, codeInternalError, "map rotation is unavailable")
		return ac, "", false
	}
	limiter := a.saasAdminReadLimiter
	if write {
		limiter = a.saasAdminActionLimiter
	}
	if !enforceRateLimit(w, limiter, rateLimitKey(r)) {
		return ac, "", false
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	reason, planBlocked, err := a.mapRotationAvailable(ctx, ac.scope.OrganizationID, ac.scope.InstallationID)
	if err != nil {
		slog.Warn("component=map_rotation", "event", "plan_lookup_failed", "installation_id", ac.scope.InstallationID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not verify your plan")
		return ac, "", false
	}
	if reason == "" && (ac.scope.ServerID == nil || *ac.scope.ServerID <= 0) {
		reason = "Select a DayZ server for this installation first."
		if write {
			writeSaaSError(w, codeInvalidRequest, "select a DayZ server first")
			return ac, "", false
		}
	}
	if write && reason != "" {
		if planBlocked {
			writeSaaSError(w, codePlanFeatureRequired, planFeatureRequiredMessage(entitlements.MapRotation))
		} else {
			writeSaaSError(w, codeForbidden, reason)
		}
		return ac, "", false
	}
	return ac, reason, true
}

func (a *App) writeMapRotationAdmin(w http.ResponseWriter, ctx context.Context, installationID int64, reason string) {
	snap, err := a.MapRotation.Load(ctx, installationID)
	if err != nil {
		slog.Warn("component=map_rotation", "event", "load_failed", "installation_id", installationID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load map rotation")
		return
	}
	writeSaaSJSON(w, http.StatusOK, toMapRotationAdminDTO(snap, reason, time.Now().UTC()))
}

func (a *App) handleAdminMapRotation(w http.ResponseWriter, r *http.Request) {
	ac, reason, ok := a.mapRotationAdmin(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	a.writeMapRotationAdmin(w, ctx, ac.scope.InstallationID, reason)
}

type mapRotationMapBody struct {
	ID        *int64 `json:"id"`
	Name      string `json:"name"`
	MapFile   string `json:"mapFile"`
	SpawnFile string `json:"spawnFile"`
	// SpawnXML is the text of the uploaded spawn file. Left out, the contents already stored for
	// the map are kept.
	SpawnXML *string `json:"spawnXml"`
	ImageURL *string `json:"imageUrl"`
	// ImageData is an uploaded picture in standard base64 (JPEG, PNG or WebP, at most
	// MaxMapImageBytes decoded). Left out or empty, the picture already stored is kept.
	ImageData *string `json:"imageData"`
	// RemoveImage clears the stored picture (a picture sent in the same save wins).
	RemoveImage bool `json:"removeImage"`
	Enabled     bool `json:"enabled"`
}

// MaxMapImageBytes is the largest picture a map may have, decoded.
const MaxMapImageBytes = 400 << 10

// mapImageBase64Max is MaxMapImageBytes as standard base64 text.
const mapImageBase64Max = (MaxMapImageBytes + 2) / 3 * 4

// mapRotationMaxBody bounds a save: every map may bring a spawn file of maprotation.MaxSpawnBytes
// and a picture of MaxMapImageBytes. As a JSON string a file is larger than on disk (every quote,
// tab and line break takes two bytes), so half as much again is allowed for that; a picture comes
// as base64. Plus the settings and the other fields.
const mapRotationMaxBody = maprotation.MaxMaps*(maprotation.MaxSpawnBytes+maprotation.MaxSpawnBytes/2+mapImageBase64Max) + 64<<10

// checkMapImage decodes an uploaded picture and returns its bytes, content type and version, or
// what is wrong with it in plain words. Only JPEG, PNG and WebP are pictures here: the type comes
// from the bytes themselves, never from what the sender says.
func checkMapImage(encoded string) (data []byte, contentType, version, problem string) {
	if len(encoded) > mapImageBase64Max {
		return nil, "", "", "the picture is too large (at most 400 KB)"
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 {
		return nil, "", "", "the picture could not be read; upload it again"
	}
	if len(data) > MaxMapImageBytes {
		return nil, "", "", "the picture is too large (at most 400 KB)"
	}
	contentType = http.DetectContentType(data)
	switch contentType {
	case "image/jpeg", "image/png":
	case "image/webp":
		if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
			return nil, "", "", "the picture must be a JPEG, PNG or WebP image"
		}
	default:
		return nil, "", "", "the picture must be a JPEG, PNG or WebP image"
	}
	sum := sha256.Sum256(data)
	return data, contentType, hex.EncodeToString(sum[:])[:16], ""
}

// mapLabel names a map in a validation message: "Map 2 (Dust)".
func mapLabel(index int, name string) string {
	return fmt.Sprintf("Map %d (%s)", index+1, name)
}

type mapRotationBody struct {
	Enabled                  bool                  `json:"enabled"`
	EveryRestarts            int                   `json:"everyRestarts"`
	Order                    string                `json:"order"`
	VoteEnabled              bool                  `json:"voteEnabled"`
	VoteMinutesBeforeRestart int                   `json:"voteMinutesBeforeRestart"`
	PingEveryone             bool                  `json:"pingEveryone"`
	AnnounceChannelID        *string               `json:"announceChannelId"`
	WipeCharacters           bool                  `json:"wipeCharacters"` // left out means off
	Maps                     *[]mapRotationMapBody `json:"maps"`
}

// validateMapRotationBody turns the request into a save, or says in plain words what is wrong.
func validateMapRotationBody(b mapRotationBody) (repository.MapRotationInput, string) {
	in := repository.MapRotationInput{Enabled: b.Enabled, EveryRestarts: b.EveryRestarts, Order: strings.ToUpper(strings.TrimSpace(b.Order)),
		VoteEnabled: b.VoteEnabled, VoteMinutes: b.VoteMinutesBeforeRestart, PingEveryone: b.PingEveryone, WipeCharacters: b.WipeCharacters}
	if in.EveryRestarts < 1 || in.EveryRestarts > 3 {
		return in, "everyRestarts must be 1, 2 or 3"
	}
	if in.Order != maprotation.OrderSequence && in.Order != maprotation.OrderRandom {
		return in, "order must be SEQUENCE or RANDOM"
	}
	if in.VoteMinutes < maprotation.MinVoteMinutes || in.VoteMinutes > maprotation.MaxVoteMinutes {
		return in, "voteMinutesBeforeRestart must be between 5 and 120"
	}
	if b.AnnounceChannelID != nil {
		if id := strings.TrimSpace(*b.AnnounceChannelID); id != "" {
			if len(id) > 32 || strings.ContainsAny(id, " \t\r\n/\\") {
				return in, "announceChannelId is not a Discord channel"
			}
			in.AnnounceChannelID = &id
		}
	}
	if b.Maps == nil {
		return in, "maps is required (send an empty list to remove every map)"
	}
	if len(*b.Maps) > maprotation.MaxMaps {
		return in, fmt.Sprintf("a rotation has at most %d maps", maprotation.MaxMaps)
	}
	enabled := 0
	pairs := map[string]bool{}
	for i, m := range *b.Maps {
		label := fmt.Sprintf("map %d", i+1)
		mi := repository.MapRotationMapInput{Name: strings.TrimSpace(m.Name), MapFile: strings.TrimSpace(m.MapFile), SpawnFile: strings.TrimSpace(m.SpawnFile), Enabled: m.Enabled}
		if m.ID != nil {
			if *m.ID <= 0 {
				return in, label + ": id is not valid"
			}
			mi.ID = *m.ID
		}
		if n := utf8.RuneCountInString(mi.Name); n < 1 || n > 60 || strings.ContainsAny(mi.Name, "\r\n\t") {
			return in, label + ": name must be 1 to 60 characters"
		}
		if err := maprotation.ValidateMapFile(mi.MapFile); err != nil {
			return in, label + ": mapFile: " + err.Error()
		}
		if err := maprotation.ValidateSpawnFile(mi.SpawnFile); err != nil {
			return in, label + ": spawnFile: " + err.Error()
		}
		if m.SpawnXML != nil {
			named := mapLabel(i, mi.Name)
			switch {
			case len(*m.SpawnXML) == 0:
				return in, named + ": the spawn file is empty"
			case len(*m.SpawnXML) > maprotation.MaxSpawnBytes:
				return in, named + ": the spawn file is too large (at most 1 MB)"
			}
			mi.SpawnXML = []byte(*m.SpawnXML)
			if err := maprotation.ValidateSpawnXML(mi.SpawnXML); err != nil {
				return in, named + ": " + err.Error()
			}
		}
		if m.ImageURL != nil {
			if raw := strings.TrimSpace(*m.ImageURL); raw != "" {
				u, err := url.Parse(raw)
				if err != nil || len(raw) > 500 || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.ContainsAny(raw, " \t\r\n\"<>") {
					return in, label + ": imageUrl must be an https address of at most 500 characters"
				}
				mi.ImageURL = &raw
			}
		}
		mi.RemoveImage = m.RemoveImage
		if m.ImageData != nil && *m.ImageData != "" {
			var problem string
			if mi.ImageData, mi.ImageType, mi.ImageVersion, problem = checkMapImage(*m.ImageData); problem != "" {
				return in, mapLabel(i, mi.Name) + ": " + problem
			}
		}
		key := strings.ToLower(mi.MapFile + "|" + mi.SpawnFile)
		if pairs[key] {
			return in, label + ": another map already uses the same two files"
		}
		pairs[key] = true
		if mi.Enabled {
			enabled++
		}
		in.Maps = append(in.Maps, mi)
	}
	if in.Enabled && enabled < maprotation.MinEnabledMaps {
		return in, "switching the rotation on needs at least 2 enabled maps"
	}
	return in, ""
}

func (a *App) handleSaveMapRotation(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.mapRotationAdmin(w, r, true)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, mapRotationMaxBody)
	var body mapRotationBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeSaaSError(w, codeValidationError, "the request is too large: a spawn file is at most 1 MB and a picture at most 400 KB")
			return
		}
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	in, problem := validateMapRotationBody(body)
	if problem != "" {
		writeSaaSError(w, codeValidationError, problem)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	before, err := a.MapRotation.Load(ctx, ac.scope.InstallationID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load map rotation")
		return
	}
	// A new announce channel must be a text channel of this installation's own Discord server:
	// Champion posts there, and may ping @everyone.
	if in.AnnounceChannelID != nil && (before.Settings.AnnounceChannelID == nil || *before.Settings.AnnounceChannelID != *in.AnnounceChannelID) {
		if a.saasDiscordVerifier == nil || ac.scope.DiscordGuildID == "" {
			writeSaaSError(w, codeDiscordUnavailable, "Discord bot session is unavailable")
			return
		}
		channels, err := a.saasDiscordVerifier.ListGuildChannels(ac.scope.DiscordGuildID)
		if err != nil {
			writeSaaSError(w, codeDiscordUnavailable, "could not check your Discord channels")
			return
		}
		found := false
		for _, c := range channels {
			found = found || c.ID == *in.AnnounceChannelID
		}
		if !found {
			writeSaaSError(w, codeValidationError, "pick a text channel in this Discord server")
			return
		}
	}
	now := time.Now().UTC()
	err = a.MapRotation.Save(ctx, ac.scope.InstallationID, in, ac.user.DiscordUserID, now)
	if errors.Is(err, repository.ErrMapRotationUnknownMap) {
		writeSaaSError(w, codeValidationError, "one of the maps has an id that is not part of this server's rotation")
		return
	}
	var noSpawns *repository.MapRotationSpawnMissingError
	if errors.As(err, &noSpawns) {
		writeSaaSError(w, codeValidationError, mapLabel(noSpawns.Index, noSpawns.Name)+": upload a spawn file")
		return
	}
	if err != nil {
		slog.Warn("component=map_rotation", "event", "save_failed", "installation_id", ac.scope.InstallationID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not save map rotation")
		return
	}
	after, err := a.MapRotation.Load(ctx, ac.scope.InstallationID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load map rotation")
		return
	}
	a.recordAudit(ctx, ac, "MAP_ROTATION_SAVE", fmt.Sprintf("installation:%d", ac.scope.InstallationID), "", "success",
		toMapRotationAdminDTO(before, "", now), toMapRotationAdminDTO(after, "", now))
	writeSaaSJSON(w, http.StatusOK, toMapRotationAdminDTO(after, "", now))
}

// handleCheckMapRotation lists the server's custom folder and reports which configured map files
// are there. It only lists a folder; nothing is downloaded and nothing is written. The spawn files
// are not on the server: for them the check reports whether the contents are stored in Champion.
func (a *App) handleCheckMapRotation(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.mapRotationAdmin(w, r, true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	snap, err := a.MapRotation.Load(ctx, ac.scope.InstallationID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load map rotation")
		return
	}
	target, err := a.MapRotation.Target(ctx, ac.scope.InstallationID)
	if err != nil || target == nil || target.OrganizationID != ac.scope.OrganizationID {
		writeSaaSError(w, codeInvalidRequest, "select a DayZ server first")
		return
	}
	remote, err := a.mapRotationRemote(ctx, *target)
	if err != nil {
		writeSaaSError(w, codeNitradoUnavailable, "could not reach Nitrado")
		return
	}
	paths, err := maprotation.Locate(ctx, remote, target.NitradoServiceID)
	if err != nil {
		writeSaaSError(w, codeNitradoUnavailable, "could not read the server's mission folder on Nitrado")
		return
	}
	files, err := maprotation.ListFiles(ctx, remote, target.NitradoServiceID, paths.CustomDir)
	if err != nil {
		// No custom folder (or it cannot be listed): every file is reported as not found.
		files = map[string]int64{}
	}
	now := time.Now().UTC()
	checks := map[int64]repository.MapFileCheck{}
	for _, m := range snap.Maps {
		_, mapOK := files[m.MapFile]
		checks[m.ID] = repository.MapFileCheck{MapFileFound: mapOK, SpawnFileFound: m.SpawnBytes > 0}
	}
	if err := a.MapRotation.SaveFileChecks(ctx, ac.scope.InstallationID, checks, now); err != nil {
		writeSaaSError(w, codeInternalError, "could not save the file check")
		return
	}
	a.recordAudit(ctx, ac, "MAP_ROTATION_FILES_CHECK", fmt.Sprintf("installation:%d", ac.scope.InstallationID), "", "success", nil, checks)
	a.writeMapRotationAdmin(w, ctx, ac.scope.InstallationID, "")
}

type mapRotationNextBody struct {
	MapID *int64 `json:"mapId"`
}

// handleSetNextMap stores (or clears) a staff member's choice for the next switch only.
func (a *App) handleSetNextMap(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.mapRotationAdmin(w, r, true)
	if !ok {
		return
	}
	body, ok := decodeJSONBody[mapRotationNextBody](w, r)
	if !ok {
		return
	}
	if body.MapID != nil && *body.MapID <= 0 {
		writeSaaSError(w, codeValidationError, "mapId is not valid")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	err := a.MapRotation.SetStaffNext(ctx, ac.scope.InstallationID, body.MapID)
	if errors.Is(err, repository.ErrMapRotationUnknownMap) {
		writeSaaSError(w, codeValidationError, "pick one of this server's enabled maps")
		return
	}
	if err != nil {
		slog.Warn("component=map_rotation", "event", "staff_next_failed", "installation_id", ac.scope.InstallationID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not set the next map")
		return
	}
	a.recordAudit(ctx, ac, "MAP_ROTATION_NEXT_SET", fmt.Sprintf("installation:%d", ac.scope.InstallationID), "", "success", nil, body)
	a.writeMapRotationAdmin(w, ctx, ac.scope.InstallationID, "")
}

// --- player ------------------------------------------------------------------------------------------

// mapVoteScope is the player preamble for the vote page: service auth, the acting user and the
// installation. Unlike the other player routes a link is not required to look (the page tells an
// unlinked visitor to link their gamertag); it is required to vote.
func (a *App) mapVoteScope(w http.ResponseWriter, r *http.Request) (repository.PlayerInstallationScope, bool, context.Context, context.CancelFunc, bool) {
	none := repository.PlayerInstallationScope{}
	if !a.requireSaaSServiceAuth(w, r) {
		return none, false, nil, nil, false
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return none, false, nil, nil, false
	}
	installationID, ok := pathInt64(w, r, "installationID")
	if !ok {
		return none, false, nil, nil, false
	}
	if a.SaaSPlayer == nil || a.MapRotation == nil {
		writeSaaSError(w, codeInternalError, "map rotation is unavailable")
		return none, false, nil, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), playerTimeout)
	scope, found, linked, err := a.SaaSPlayer.ResolvePlayerInstallation(ctx, installationID, user.DiscordUserID)
	if err != nil {
		cancel()
		playerFailed(w, "map vote", err)
		return none, false, nil, nil, false
	}
	if !found {
		cancel()
		writeSaaSError(w, codeNotFound, "installation not found")
		return none, false, nil, nil, false
	}
	return scope, linked, ctx, cancel, true
}

// mapRotationPlayerView builds what a player sees. With the feature off for the installation the
// view is `enabled: false` and nothing else.
func (a *App) mapRotationPlayerView(ctx context.Context, scope repository.PlayerInstallationScope, linked bool) (mapRotationPlayerDTO, error) {
	out := mapRotationPlayerDTO{Linked: linked, ServerName: scope.ServerName}
	reason, _, err := a.mapRotationAvailable(ctx, scope.OrganizationID, scope.InstallationID)
	if err != nil {
		return out, err
	}
	if reason != "" {
		return out, nil
	}
	snap, err := a.MapRotation.Load(ctx, scope.InstallationID)
	if err != nil {
		return out, err
	}
	s := snap.Settings
	if !s.Enabled {
		return out, nil
	}
	out.Enabled = true
	if s.CurrentMapID != nil && s.CurrentMapName != nil {
		out.Current = &mapPlayerCurrentDTO{MapID: *s.CurrentMapID, Name: *s.CurrentMapName}
	}
	if m, by, ok := mapRotationNext(s, rotationMaps(snap.Maps)); ok {
		out.Next = &mapPlayerNextDTO{MapID: m.ID, Name: m.Name, DecidedBy: by}
	}
	if snap.Vote != nil {
		out.Vote = &mapPlayerVoteDTO{mapVoteDTO: *toMapVoteDTO(snap.Vote)}
		if linked {
			if out.Vote.MyVote, err = a.MapRotation.Ballot(ctx, snap.Vote.ID, scope.PlayerID); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

// handlePublicMapImage is GET /api/saas/network/servers/{installationID}/map-images/{mapID}
// (bearer only, like the public live map): the picture stored for a map, as bytes. It does not
// depend on the rotation being on or on the plan - a picture that was uploaded can be shown. A
// missing installation, map or picture, and a map of another installation, are the same 404. The
// version in the ETag changes with the bytes, so the answer may be cached for good.
func (a *App) handlePublicMapImage(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	load := a.mapRotationImageFor
	if load == nil {
		if a.MapRotation == nil {
			writeSaaSError(w, codeInternalError, "map rotation is unavailable")
			return
		}
		load = a.MapRotation.MapImage
	}
	installationID, ok := pathInt64(w, r, "installationID")
	if !ok {
		return
	}
	mapID, ok := pathInt64(w, r, "mapID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), networkTimeout)
	defer cancel()
	img, err := load(ctx, installationID, mapID)
	if err != nil {
		slog.Warn("component=map_rotation", "event", "image_load_failed", "installation_id", installationID, "map_id", mapID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the picture")
		return
	}
	if img == nil || len(img.Data) == 0 {
		writeSaaSError(w, codeNotFound, "picture not found")
		return
	}
	h := w.Header()
	h.Set("Content-Type", img.ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", `"`+img.Version+`"`)
	h.Set("Content-Length", strconv.Itoa(len(img.Data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(img.Data)
}

// handlePlayerMapVote is GET .../player/servers/{installationID}/map/vote.
func (a *App) handlePlayerMapVote(w http.ResponseWriter, r *http.Request) {
	scope, linked, ctx, cancel, ok := a.mapVoteScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	if !enforceRateLimit(w, a.saasLiveMapLimiter, rateLimitKey(r)) {
		return
	}
	view, err := a.mapRotationPlayerView(ctx, scope, linked)
	if err != nil {
		playerFailed(w, "map vote", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, view)
}

type mapVoteBody struct {
	MapID int64 `json:"mapId"`
}

// handleCastMapVote is POST .../player/servers/{installationID}/map/vote: one vote per linked
// player per vote; voting again changes it.
func (a *App) handleCastMapVote(w http.ResponseWriter, r *http.Request) {
	scope, linked, ctx, cancel, ok := a.mapVoteScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	if !enforceRateLimit(w, a.saasLiveMapLimiter, rateLimitKey(r)) {
		return
	}
	if !linked {
		writeSaaSError(w, codeNotLinked, "link your gamertag to vote")
		return
	}
	body, ok := decodeJSONBody[mapVoteBody](w, r)
	if !ok {
		return
	}
	if body.MapID <= 0 {
		writeSaaSError(w, codeValidationError, "pick one of the maps in the vote")
		return
	}
	reason, _, err := a.mapRotationAvailable(ctx, scope.OrganizationID, scope.InstallationID)
	if err != nil {
		playerFailed(w, "map vote", err)
		return
	}
	if reason != "" {
		writeSaaSError(w, codeVoteClosed, "there is no open vote")
		return
	}
	err = a.MapRotation.CastBallot(ctx, scope.InstallationID, scope.PlayerID, body.MapID, time.Now().UTC())
	switch {
	case errors.Is(err, repository.ErrMapVoteClosed):
		writeSaaSError(w, codeVoteClosed, "there is no open vote")
		return
	case errors.Is(err, repository.ErrMapRotationUnknownMap):
		writeSaaSError(w, codeValidationError, "pick one of the maps in the vote")
		return
	case err != nil:
		playerFailed(w, "map vote", err)
		return
	}
	view, err := a.mapRotationPlayerView(ctx, scope, linked)
	if err != nil {
		playerFailed(w, "map vote", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, view)
}
