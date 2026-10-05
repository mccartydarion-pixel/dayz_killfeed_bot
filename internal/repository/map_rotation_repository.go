package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Map rotation with a player vote (docs/MAP_ROTATION.md). This repository only stores settings,
// rotation state, votes and the switch log; the rules live in internal/maprotation and the file
// writes in internal/maprotation/mapswitch.

var (
	ErrMapRotationUnknownMap = errors.New("that map is not part of this server's rotation")
	ErrMapVoteClosed         = errors.New("there is no open vote")
	ErrMapSwitchNotPending   = errors.New("the switch is not pending")
)

// MapRotationSpawnMissingError: a save left out the spawn contents of a map that has none stored.
// Index is the map's place in the save (0-based).
type MapRotationSpawnMissingError struct {
	Index int
	Name  string
}

func (e *MapRotationSpawnMissingError) Error() string {
	return "a map has no spawn file stored and none was sent"
}

// Switch statuses.
const (
	MapSwitchPending    = "PENDING"
	MapSwitchApplied    = "APPLIED"
	MapSwitchFailed     = "FAILED"
	MapSwitchRolledBack = "ROLLED_BACK"
)

// MapRotationMaxFailures: after this many failed switches in a row the rotation stops until the
// owner saves the settings again.
const MapRotationMaxFailures = 3

type MapRotationMap struct {
	ID             int64
	Name           string
	MapFile        string
	SpawnFile      string // the name of the uploaded spawn file, for display
	SpawnBytes     int    // size of the stored spawn contents, 0 when none are stored
	ImageURL       *string
	ImageBytes     int     // size of the stored picture, 0 when none is stored
	ImageVersion   *string // the stored picture's version (nil when none is stored)
	Enabled        bool
	Position       int
	MapFileFound   *bool
	SpawnFileFound *bool
	CheckedAt      *time.Time
}

// MapRotationSettings is the owner's settings plus Champion's rotation state.
type MapRotationSettings struct {
	InstallationID    int64
	Enabled           bool
	EveryRestarts     int
	Order             string
	VoteEnabled       bool
	VoteMinutes       int
	PingEveryone      bool
	AnnounceChannelID *string
	// WipeCharacters: clear the saved characters after every map switch (off unless switched on).
	WipeCharacters bool

	CurrentMapID        *int64
	CurrentMapName      *string
	CurrentMapFile      *string
	CurrentSince        *time.Time
	NextMapID           *int64
	NextDecidedBy       *string
	StaffNextMapID      *int64
	LastBootFile        string
	RestartsSinceSwitch int
	Phase               string
	PhaseStartedAt      *time.Time
	NextRestartAt       *time.Time
	ConsecutiveFailures int
	HaltedReason        string
	// WipeRestartAt is when Champion last started the server itself after clearing characters.
	WipeRestartAt *time.Time
}

func defaultMapRotationSettings(installationID int64) MapRotationSettings {
	return MapRotationSettings{InstallationID: installationID, EveryRestarts: 1, Order: "SEQUENCE", VoteMinutes: 30, Phase: "IDLE"}
}

type MapRotationVoteOption struct {
	MapID    int64
	Name     string
	ImageURL *string
	// ImageVersion is the version of the picture stored for the map now (read from the map's row
	// when the vote is loaded, not kept with the option); nil when none is stored.
	ImageVersion *string
	Position     int
	Votes        int
}

type MapRotationVote struct {
	ID                int64
	Status            string
	OpensAt, ClosesAt time.Time
	ClosedAt          *time.Time
	WinnerMapID       *int64
	WinnerName        string
	DecidedBy         *string
	Options           []MapRotationVoteOption
	TotalVotes        int
}

type MapRotationSwitch struct {
	ID           int64
	MapID        *int64
	MapName      string
	MapFile      string
	SpawnFile    string
	DecidedBy    string
	Status       string
	Message      string
	Attempts     int
	PrevGameplay []byte // loaded only by PendingSwitch and BeginSwitch
	PrevSpawns   []byte
	// SpawnXML is the map's spawn contents as they were when the switch began (loaded only by
	// PendingSwitch and BeginSwitch; empty when the map had none, and after the switch finished).
	SpawnXML    []byte
	BackupSaved bool
	CreatedAt   time.Time
	FinishedAt  *time.Time
	// Clearing the saved characters after this switch (loaded by PendingSwitch and BeginSwitch):
	// how far it got (MapWipe...), when, and the outcome as a sentence.
	WipeState   string
	WipeStateAt *time.Time
	WipeNote    string
	// CharactersCleared: nil when clearing was not attempted, else whether players.db was verified
	// deleted. Also loaded for the snapshot's LastSwitch.
	CharactersCleared *bool
}

// States of clearing the saved characters after a switch (the same words as
// internal/maprotation/charwipe, which owns the procedure).
const (
	MapWipeNone           = "NONE"
	MapWipeStopRequested  = "STOP_REQUESTED"
	MapWipeDeleted        = "DELETED"
	MapWipeStartRequested = "START_REQUESTED"
	MapWipeDone           = "DONE"
	MapWipeFailed         = "FAILED"
)

// MapRotationSnapshot is everything the API shows.
type MapRotationSnapshot struct {
	Settings   MapRotationSettings
	Maps       []MapRotationMap
	Vote       *MapRotationVote   // the open vote, or the vote that decided this period
	LastSwitch *MapRotationSwitch // the newest finished switch
}

// MapRotationMapInput is one map of a save. ID 0 creates a map.
type MapRotationMapInput struct {
	ID        int64
	Name      string
	MapFile   string
	SpawnFile string
	// SpawnXML is the uploaded spawn file. nil keeps the contents already stored for the map; a map
	// with none stored must bring them.
	SpawnXML []byte
	ImageURL *string
	// ImageData is an uploaded picture with its content type and version. nil keeps the picture
	// already stored for the map; RemoveImage clears it (a picture sent in the same save wins).
	ImageData    []byte
	ImageType    string
	ImageVersion string
	RemoveImage  bool
	Enabled      bool
}

// MapImage is a map's stored picture as it is served.
type MapImage struct {
	Data        []byte
	ContentType string
	Version     string
}

// MapRotationInput is a save. The caller has validated it.
type MapRotationInput struct {
	Enabled           bool
	EveryRestarts     int
	Order             string
	VoteEnabled       bool
	VoteMinutes       int
	PingEveryone      bool
	AnnounceChannelID *string
	WipeCharacters    bool
	Maps              []MapRotationMapInput
}

// MapRotationTarget is one installation the worker looks at.
type MapRotationTarget struct {
	InstallationID   int64
	OrganizationID   int64
	GuildID          int64
	ServerID         int64
	NitradoServiceID string
	ServerName       string
}

type MapRotationRepository struct{ pool *pgxpool.Pool }

func NewMapRotationRepository(pool *pgxpool.Pool) *MapRotationRepository {
	return &MapRotationRepository{pool: pool}
}

type mapRotationQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const mapRotationSettingsColumns = `enabled, every_restarts, rotation_order, vote_enabled, vote_minutes, ping_everyone, announce_channel_id,
 current_map_id, current_map_name, current_map_file, current_since, next_map_id, next_decided_by, staff_next_map_id,
 last_boot_file, restarts_since_switch, phase, phase_started_at, next_restart_at, consecutive_failures, halted_reason,
 wipe_characters, wipe_restart_at`

func loadMapRotationSettings(ctx context.Context, q mapRotationQuerier, installationID int64) (MapRotationSettings, error) {
	s := defaultMapRotationSettings(installationID)
	err := q.QueryRow(ctx, `SELECT `+mapRotationSettingsColumns+` FROM map_rotation_settings WHERE installation_id=$1`, installationID).Scan(
		&s.Enabled, &s.EveryRestarts, &s.Order, &s.VoteEnabled, &s.VoteMinutes, &s.PingEveryone, &s.AnnounceChannelID,
		&s.CurrentMapID, &s.CurrentMapName, &s.CurrentMapFile, &s.CurrentSince, &s.NextMapID, &s.NextDecidedBy, &s.StaffNextMapID,
		&s.LastBootFile, &s.RestartsSinceSwitch, &s.Phase, &s.PhaseStartedAt, &s.NextRestartAt, &s.ConsecutiveFailures, &s.HaltedReason,
		&s.WipeCharacters, &s.WipeRestartAt)
	if errors.Is(err, pgx.ErrNoRows) {
		s.PingEveryone = true // the vote ping reaches everyone unless the owner turns it off
		return s, nil
	}
	return s, err
}

func loadMapRotationMaps(ctx context.Context, q mapRotationQuerier, installationID int64) ([]MapRotationMap, error) {
	rows, err := q.Query(ctx, `SELECT id, name, map_file, spawn_file, COALESCE(octet_length(spawn_xml), 0)::int, image_url, COALESCE(octet_length(image_data), 0)::int, image_version, enabled, position, map_file_found, spawn_file_found, checked_at
FROM map_rotation_maps WHERE installation_id=$1 ORDER BY position, id`, installationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MapRotationMap{}
	for rows.Next() {
		var m MapRotationMap
		if err := rows.Scan(&m.ID, &m.Name, &m.MapFile, &m.SpawnFile, &m.SpawnBytes, &m.ImageURL, &m.ImageBytes, &m.ImageVersion, &m.Enabled, &m.Position, &m.MapFileFound, &m.SpawnFileFound, &m.CheckedAt); err != nil {
			return nil, err
		}
		if m.ImageBytes == 0 {
			m.ImageVersion = nil
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func loadMapRotationVote(ctx context.Context, q mapRotationQuerier, where string, args ...any) (*MapRotationVote, error) {
	var v MapRotationVote
	err := q.QueryRow(ctx, `SELECT id, status, opens_at, closes_at, closed_at, winner_map_id, winner_name, decided_by FROM map_rotation_votes WHERE `+where+` ORDER BY id DESC LIMIT 1`, args...).
		Scan(&v.ID, &v.Status, &v.OpensAt, &v.ClosesAt, &v.ClosedAt, &v.WinnerMapID, &v.WinnerName, &v.DecidedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT o.map_id, o.name, o.image_url, o.position, (SELECT COUNT(*) FROM map_rotation_ballots b WHERE b.vote_id=o.vote_id AND b.map_id=o.map_id)::int,
    (SELECT m.image_version FROM map_rotation_maps m WHERE m.id=o.map_id AND m.image_data IS NOT NULL)
FROM map_rotation_vote_options o WHERE o.vote_id=$1 ORDER BY o.position, o.map_id`, v.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	v.Options = []MapRotationVoteOption{}
	for rows.Next() {
		var o MapRotationVoteOption
		if err := rows.Scan(&o.MapID, &o.Name, &o.ImageURL, &o.Position, &o.Votes, &o.ImageVersion); err != nil {
			return nil, err
		}
		v.TotalVotes += o.Votes
		v.Options = append(v.Options, o)
	}
	return &v, rows.Err()
}

// Load reads the settings (defaults when the owner never saved), the maps, the vote players see
// and the newest finished switch.
func (r *MapRotationRepository) Load(ctx context.Context, installationID int64) (MapRotationSnapshot, error) {
	var snap MapRotationSnapshot
	var err error
	if snap.Settings, err = loadMapRotationSettings(ctx, r.pool, installationID); err != nil {
		return snap, err
	}
	if snap.Maps, err = loadMapRotationMaps(ctx, r.pool, installationID); err != nil {
		return snap, err
	}
	if snap.Vote, err = loadMapRotationVote(ctx, r.pool, `installation_id=$1 AND status='OPEN'`, installationID); err != nil {
		return snap, err
	}
	// Until the restart, the vote that decided this period stays visible as CLOSED.
	if snap.Vote == nil && snap.Settings.LastBootFile != "" && (snap.Settings.Phase == "DECIDED" || snap.Settings.Phase == "DONE") {
		if snap.Vote, err = loadMapRotationVote(ctx, r.pool, `installation_id=$1 AND status='CLOSED' AND boot_file=$2 AND winner_map_id IS NOT NULL`, installationID, snap.Settings.LastBootFile); err != nil {
			return snap, err
		}
	}
	var sw MapRotationSwitch
	err = r.pool.QueryRow(ctx, `SELECT id, map_id, map_name, map_file, spawn_file, decided_by, status, message, attempts, created_at, finished_at, characters_cleared
FROM map_rotation_switches WHERE installation_id=$1 AND status<>'PENDING' ORDER BY id DESC LIMIT 1`, installationID).
		Scan(&sw.ID, &sw.MapID, &sw.MapName, &sw.MapFile, &sw.SpawnFile, &sw.DecidedBy, &sw.Status, &sw.Message, &sw.Attempts, &sw.CreatedAt, &sw.FinishedAt, &sw.CharactersCleared)
	if err == nil {
		snap.LastSwitch = &sw
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return snap, err
	}
	return snap, nil
}

// MapImage returns the picture stored for a map of the installation, or nil when the map does not
// exist, belongs to another installation or has no picture.
func (r *MapRotationRepository) MapImage(ctx context.Context, installationID, mapID int64) (*MapImage, error) {
	var img MapImage
	err := r.pool.QueryRow(ctx, `SELECT image_data, COALESCE(image_type, ''), COALESCE(image_version, '') FROM map_rotation_maps
WHERE id=$1 AND installation_id=$2 AND image_data IS NOT NULL`, mapID, installationID).Scan(&img.Data, &img.ContentType, &img.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(img.Data) == 0 || img.ContentType == "" || img.Version == "" {
		return nil, nil
	}
	return &img, nil
}

const ensureMapRotationRow = `INSERT INTO map_rotation_settings(installation_id) VALUES($1) ON CONFLICT (installation_id) DO NOTHING`

// Save stores the owner's settings and the list of maps (list order = rotation order). A map with
// an ID is updated, one without is created, and a stored map that is not listed is removed. Saving
// also clears a stop caused by failed switches. A map's spawn contents are replaced when the save
// brings them and kept otherwise; a map that would end up with none refuses the whole save with a
// *MapRotationSpawnMissingError.
func (r *MapRotationRepository) Save(ctx context.Context, installationID int64, in MapRotationInput, actor string, now time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, ensureMapRotationRow, installationID); err != nil {
		return err
	}
	// Lock the row: the worker and a save never interleave.
	var wasEnabled bool
	if err := tx.QueryRow(ctx, `SELECT enabled FROM map_rotation_settings WHERE installation_id=$1 FOR UPDATE`, installationID).Scan(&wasEnabled); err != nil {
		return err
	}
	existing := map[int64]MapRotationMap{}
	stored, err := loadMapRotationMaps(ctx, tx, installationID)
	if err != nil {
		return err
	}
	for _, m := range stored {
		existing[m.ID] = m
	}
	keep := map[int64]bool{}
	for i, m := range in.Maps {
		if m.ID != 0 {
			old, ok := existing[m.ID]
			if !ok || keep[m.ID] {
				return ErrMapRotationUnknownMap
			}
			keep[m.ID] = true
			if len(m.SpawnXML) == 0 && old.SpawnBytes == 0 {
				return &MapRotationSpawnMissingError{Index: i, Name: m.Name}
			}
			// The file check is about the map file on the server; it stands while that name does.
			sameFiles := old.MapFile == m.MapFile
			if _, err := tx.Exec(ctx, `UPDATE map_rotation_maps SET name=$3, map_file=$4, spawn_file=$5, image_url=$6, enabled=$7, position=$8,
    map_file_found = CASE WHEN $9::boolean THEN map_file_found END, spawn_file_found = CASE WHEN $9::boolean THEN spawn_file_found END, checked_at = CASE WHEN $9::boolean THEN checked_at END,
    spawn_xml = COALESCE($10::bytea, spawn_xml),
    image_data = CASE WHEN $11::bytea IS NOT NULL THEN $11::bytea WHEN $14::boolean THEN NULL ELSE image_data END,
    image_type = CASE WHEN $11::bytea IS NOT NULL THEN $12::text WHEN $14::boolean THEN NULL ELSE image_type END,
    image_version = CASE WHEN $11::bytea IS NOT NULL THEN $13::text WHEN $14::boolean THEN NULL ELSE image_version END
WHERE id=$1 AND installation_id=$2`, m.ID, installationID, m.Name, m.MapFile, m.SpawnFile, m.ImageURL, m.Enabled, i, sameFiles, spawnParam(m.SpawnXML),
				spawnParam(m.ImageData), m.ImageType, m.ImageVersion, m.RemoveImage); err != nil {
				return err
			}
			continue
		}
		if len(m.SpawnXML) == 0 {
			return &MapRotationSpawnMissingError{Index: i, Name: m.Name}
		}
		var id int64
		if err := tx.QueryRow(ctx, `INSERT INTO map_rotation_maps(installation_id, name, map_file, spawn_file, image_url, enabled, position, spawn_xml, image_data, image_type, image_version)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::bytea, CASE WHEN $9::bytea IS NOT NULL THEN $10::text END, CASE WHEN $9::bytea IS NOT NULL THEN $11::text END) RETURNING id`,
			installationID, m.Name, m.MapFile, m.SpawnFile, m.ImageURL, m.Enabled, i, m.SpawnXML, spawnParam(m.ImageData), m.ImageType, m.ImageVersion).Scan(&id); err != nil {
			return err
		}
		keep[id] = true
	}
	for id := range existing {
		if !keep[id] {
			if _, err := tx.Exec(ctx, `DELETE FROM map_rotation_maps WHERE id=$1 AND installation_id=$2`, id, installationID); err != nil {
				return err
			}
		}
	}
	// A decision for a map that is gone or switched off no longer stands. The current map keeps its
	// file name even when the map is removed, so the next switch still takes its entry out.
	if _, err := tx.Exec(ctx, `
UPDATE map_rotation_settings s SET
    enabled=$2, every_restarts=$3, rotation_order=$4, vote_enabled=$5, vote_minutes=$6, ping_everyone=$7, announce_channel_id=$8,
    updated_at=$9, updated_by=$10, wipe_characters=$11, consecutive_failures=0, halted_reason='',
    next_map_id       = CASE WHEN EXISTS(SELECT 1 FROM map_rotation_maps m WHERE m.id=s.next_map_id AND m.installation_id=$1 AND m.enabled) THEN s.next_map_id END,
    next_decided_by   = CASE WHEN EXISTS(SELECT 1 FROM map_rotation_maps m WHERE m.id=s.next_map_id AND m.installation_id=$1 AND m.enabled) THEN s.next_decided_by END,
    staff_next_map_id = CASE WHEN EXISTS(SELECT 1 FROM map_rotation_maps m WHERE m.id=s.staff_next_map_id AND m.installation_id=$1 AND m.enabled) THEN s.staff_next_map_id END,
    current_map_id    = CASE WHEN EXISTS(SELECT 1 FROM map_rotation_maps m WHERE m.id=s.current_map_id AND m.installation_id=$1) THEN s.current_map_id END
WHERE s.installation_id=$1`,
		installationID, in.Enabled, in.EveryRestarts, in.Order, in.VoteEnabled, in.VoteMinutes, in.PingEveryone, in.AnnounceChannelID, now, actor, in.WipeCharacters); err != nil {
		return err
	}
	if !in.Enabled || !in.VoteEnabled {
		if _, err := tx.Exec(ctx, `UPDATE map_rotation_votes SET status='CLOSED', closed_at=$2 WHERE installation_id=$1 AND status='OPEN'`, installationID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE map_rotation_settings SET phase='IDLE' WHERE installation_id=$1 AND phase='VOTING'`, installationID); err != nil {
			return err
		}
	}
	if !in.Enabled {
		// Switched off: forget the boot, so restarts while it is off are not counted later.
		if _, err := tx.Exec(ctx, `UPDATE map_rotation_settings SET phase='IDLE', last_boot_file='', phase_started_at=NULL, next_restart_at=NULL WHERE installation_id=$1`, installationID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// spawnParam is the spawn contents as a query argument: NULL (keep what is stored) when empty.
func spawnParam(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// SetStaffNext stores (or, with nil, clears) the map a staff member picked for the next switch.
func (r *MapRotationRepository) SetStaffNext(ctx context.Context, installationID int64, mapID *int64) error {
	if mapID != nil {
		var ok bool
		if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM map_rotation_maps WHERE id=$2 AND installation_id=$1 AND enabled)`, installationID, *mapID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return ErrMapRotationUnknownMap
		}
	}
	if _, err := r.pool.Exec(ctx, ensureMapRotationRow, installationID); err != nil {
		return err
	}
	if mapID != nil {
		_, err := r.pool.Exec(ctx, `UPDATE map_rotation_settings SET staff_next_map_id=$2 WHERE installation_id=$1`, installationID, *mapID)
		return err
	}
	// Clearing also takes back a staff decision that was made but not applied yet, so the vote or
	// the rotation decides again.
	_, err := r.pool.Exec(ctx, `UPDATE map_rotation_settings SET staff_next_map_id=NULL,
    next_map_id     = CASE WHEN next_decided_by='STAFF' AND phase IN ('IDLE','DECIDED') THEN NULL ELSE next_map_id END,
    next_decided_by = CASE WHEN next_decided_by='STAFF' AND phase IN ('IDLE','DECIDED') THEN NULL ELSE next_decided_by END,
    phase           = CASE WHEN next_decided_by='STAFF' AND phase='DECIDED' THEN 'IDLE' ELSE phase END
WHERE installation_id=$1`, installationID)
	return err
}

// MapFileCheck is whether a map's map file was found in the server's custom folder and whether its
// spawn contents are stored in Champion.
type MapFileCheck struct{ MapFileFound, SpawnFileFound bool }

func (r *MapRotationRepository) SaveFileChecks(ctx context.Context, installationID int64, checks map[int64]MapFileCheck, now time.Time) error {
	for id, c := range checks {
		if _, err := r.pool.Exec(ctx, `UPDATE map_rotation_maps SET map_file_found=$3, spawn_file_found=$4, checked_at=$5 WHERE id=$1 AND installation_id=$2`,
			id, installationID, c.MapFileFound, c.SpawnFileFound, now); err != nil {
			return err
		}
	}
	return nil
}

// Ballot is the map playerID voted for in voteID, nil when they have not voted.
func (r *MapRotationRepository) Ballot(ctx context.Context, voteID, playerID int64) (*int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT map_id FROM map_rotation_ballots WHERE vote_id=$1 AND player_id=$2`, voteID, playerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// CastBallot records playerID's vote in the installation's open vote; voting again changes it.
func (r *MapRotationRepository) CastBallot(ctx context.Context, installationID, playerID, mapID int64, now time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var voteID int64
	err = tx.QueryRow(ctx, `SELECT v.id FROM map_rotation_votes v JOIN map_rotation_settings s ON s.installation_id=v.installation_id
WHERE v.installation_id=$1 AND v.status='OPEN' AND v.closes_at > $2 AND s.enabled FOR UPDATE OF v`, installationID, now).Scan(&voteID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrMapVoteClosed
	}
	if err != nil {
		return err
	}
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM map_rotation_vote_options WHERE vote_id=$1 AND map_id=$2)`, voteID, mapID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrMapRotationUnknownMap
	}
	if _, err := tx.Exec(ctx, `INSERT INTO map_rotation_ballots(vote_id, player_id, map_id, created_at, updated_at) VALUES($1,$2,$3,$4,$4)
ON CONFLICT (vote_id, player_id) DO UPDATE SET map_id=EXCLUDED.map_id, updated_at=EXCLUDED.updated_at`, voteID, playerID, mapID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- worker --------------------------------------------------------------------------------------

// ActiveInstallations lists the installations whose owner switched the rotation on, which are not
// stopped after failed switches and which Champion has not suspended.
func (r *MapRotationRepository) ActiveInstallations(ctx context.Context) ([]MapRotationTarget, error) {
	rows, err := r.pool.Query(ctx, `
SELECT s.installation_id, i.organization_id, c.guild_id, gs.id, COALESCE(gs.provider_service_id,''), COALESCE(gs.display_name,'')
FROM map_rotation_settings s
JOIN installations i ON i.id = s.installation_id
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
JOIN game_servers gs ON gs.id = i.game_server_id
WHERE s.enabled AND s.halted_reason = '' AND i.suspended_at IS NULL ORDER BY s.installation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MapRotationTarget
	for rows.Next() {
		var t MapRotationTarget
		if err := rows.Scan(&t.InstallationID, &t.OrganizationID, &t.GuildID, &t.ServerID, &t.NitradoServiceID, &t.ServerName); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Target resolves one installation for the worker and the file check.
func (r *MapRotationRepository) Target(ctx context.Context, installationID int64) (*MapRotationTarget, error) {
	var t MapRotationTarget
	err := r.pool.QueryRow(ctx, `
SELECT i.id, i.organization_id, c.guild_id, gs.id, COALESCE(gs.provider_service_id,''), COALESCE(gs.display_name,'')
FROM installations i
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
JOIN game_servers gs ON gs.id = i.game_server_id
WHERE i.id=$1`, installationID).Scan(&t.InstallationID, &t.OrganizationID, &t.GuildID, &t.ServerID, &t.NitradoServiceID, &t.ServerName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Claim takes the installation's lease for owner until `until`, so two bot processes never work
// one installation at the same time. It reports whether the lease was taken.
func (r *MapRotationRepository) Claim(ctx context.Context, installationID int64, owner string, now, until time.Time) (bool, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE map_rotation_settings SET lease_owner=$2, lease_until=$4
WHERE installation_id=$1 AND (lease_until IS NULL OR lease_until < $3 OR lease_owner=$2)`, installationID, owner, now, until)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r *MapRotationRepository) Release(ctx context.Context, installationID int64, owner string) error {
	_, err := r.pool.Exec(ctx, `UPDATE map_rotation_settings SET lease_until=NULL, lease_owner=NULL WHERE installation_id=$1 AND lease_owner=$2`, installationID, owner)
	return err
}

// BootSession is the server's current boot as the ADM ingestion recorded it (server_adm_sessions):
// the ADM file is the identity of a boot, so a new file means the server restarted.
func (r *MapRotationRepository) BootSession(ctx context.Context, serverID int64) (file string, at time.Time, err error) {
	err = r.pool.QueryRow(ctx, `SELECT adm_file, selected_at FROM server_adm_sessions WHERE server_id=$1`, serverID).Scan(&file, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, nil
	}
	return file, at, err
}

// Baseline remembers the current boot without counting it.
func (r *MapRotationRepository) Baseline(ctx context.Context, installationID int64, bootFile string, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE map_rotation_settings SET last_boot_file=$2, phase='IDLE', phase_started_at=$3, next_restart_at=NULL WHERE installation_id=$1`, installationID, bootFile, at)
	return err
}

// RecordRestart handles a new boot. If a switch was applied and not yet active, its map becomes
// the current map and the restart count starts again (the switch is returned); otherwise the
// restart is counted. A vote still open is closed without a result.
//
// count false is for a further boot shortly after Champion's own restart (the one that follows
// clearing the characters; maprotation.RestartSame): the boot is remembered and the period begins
// with it, but the count stays as it is. A switch waiting for its restart is activated either way.
func (r *MapRotationRepository) RecordRestart(ctx context.Context, installationID int64, bootFile string, bootAt, now time.Time, count bool) (*MapRotationSwitch, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var prev string
	if err := tx.QueryRow(ctx, `SELECT last_boot_file FROM map_rotation_settings WHERE installation_id=$1 FOR UPDATE`, installationID).Scan(&prev); err != nil {
		return nil, err
	}
	if prev == bootFile {
		return nil, tx.Commit(ctx) // already recorded
	}
	var sw MapRotationSwitch
	err = tx.QueryRow(ctx, `UPDATE map_rotation_switches SET activated_at=$2
WHERE id = (SELECT id FROM map_rotation_switches WHERE installation_id=$1 AND status='APPLIED' AND activated_at IS NULL ORDER BY id DESC LIMIT 1)
RETURNING id, map_id, map_name, map_file, spawn_file, decided_by, status, message`, installationID, now).
		Scan(&sw.ID, &sw.MapID, &sw.MapName, &sw.MapFile, &sw.SpawnFile, &sw.DecidedBy, &sw.Status, &sw.Message)
	var activated *MapRotationSwitch
	switch {
	case err == nil:
		activated = &sw
		if _, err := tx.Exec(ctx, `UPDATE map_rotation_settings SET current_map_id=(SELECT m.id FROM map_rotation_maps m WHERE m.id=$2 AND m.installation_id=$1),
    current_map_name=$3, current_map_file=$4, current_since=$5, restarts_since_switch=0, next_map_id=NULL, next_decided_by=NULL
WHERE installation_id=$1`, installationID, sw.MapID, sw.MapName, sw.MapFile, bootAt); err != nil {
			return nil, err
		}
	case errors.Is(err, pgx.ErrNoRows):
		if count {
			if _, err := tx.Exec(ctx, `UPDATE map_rotation_settings SET restarts_since_switch=restarts_since_switch+1 WHERE installation_id=$1`, installationID); err != nil {
				return nil, err
			}
		}
	default:
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE map_rotation_votes SET status='CLOSED', closed_at=$2 WHERE installation_id=$1 AND status='OPEN'`, installationID, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE map_rotation_settings SET last_boot_file=$2, phase='IDLE', phase_started_at=$3, next_restart_at=NULL WHERE installation_id=$1`, installationID, bootFile, bootAt); err != nil {
		return nil, err
	}
	return activated, tx.Commit(ctx)
}

// OpenVote opens a vote over options and moves the period to VOTING. With a vote already open it
// changes nothing and returns that vote's id.
func (r *MapRotationRepository) OpenVote(ctx context.Context, installationID int64, opensAt, closesAt time.Time, bootFile string, options []MapRotationVoteOption) (int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO map_rotation_votes(installation_id, status, opens_at, closes_at, boot_file) VALUES($1,'OPEN',$2,$3,$4)
ON CONFLICT (installation_id) WHERE status = 'OPEN' DO NOTHING RETURNING id`, installationID, opensAt, closesAt, bootFile).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx, `SELECT id FROM map_rotation_votes WHERE installation_id=$1 AND status='OPEN'`, installationID).Scan(&id); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	} else {
		for i, o := range options {
			if _, err := tx.Exec(ctx, `INSERT INTO map_rotation_vote_options(vote_id, map_id, name, image_url, position) VALUES($1,$2,$3,$4,$5)`, id, o.MapID, o.Name, o.ImageURL, i); err != nil {
				return 0, err
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE map_rotation_settings SET phase='VOTING' WHERE installation_id=$1`, installationID); err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// Decide stores the next map and moves the period to DECIDED. With voteID it also closes that vote
// with the result. A staff choice is used up by the decision it made.
func (r *MapRotationRepository) Decide(ctx context.Context, installationID int64, voteID *int64, mapID int64, mapName, decidedBy string, now time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if voteID != nil {
		if _, err := tx.Exec(ctx, `UPDATE map_rotation_votes SET status='CLOSED', closed_at=$3, winner_map_id=$4, winner_name=$5, decided_by=$6 WHERE id=$2 AND installation_id=$1 AND status='OPEN'`,
			installationID, *voteID, now, mapID, mapName, decidedBy); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE map_rotation_settings SET next_map_id=$2, next_decided_by=$3::text, phase='DECIDED',
    staff_next_map_id = CASE WHEN $3::text='STAFF' THEN NULL ELSE staff_next_map_id END WHERE installation_id=$1`, installationID, mapID, decidedBy); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// VoteCounts is the number of ballots per map of a vote.
func (r *MapRotationRepository) VoteCounts(ctx context.Context, voteID int64) (map[int64]int, error) {
	rows, err := r.pool.Query(ctx, `SELECT map_id, COUNT(*)::int FROM map_rotation_ballots WHERE vote_id=$1 GROUP BY map_id`, voteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

const mapSwitchColumns = `id, map_id, map_name, map_file, spawn_file, decided_by, status, message, attempts, prev_gameplay, prev_spawns, backup_saved_at IS NOT NULL, created_at, finished_at, spawn_xml,
 wipe_state, wipe_state_at, wipe_note, characters_cleared`

func scanMapSwitch(row pgx.Row) (*MapRotationSwitch, error) {
	var sw MapRotationSwitch
	err := row.Scan(&sw.ID, &sw.MapID, &sw.MapName, &sw.MapFile, &sw.SpawnFile, &sw.DecidedBy, &sw.Status, &sw.Message, &sw.Attempts, &sw.PrevGameplay, &sw.PrevSpawns, &sw.BackupSaved, &sw.CreatedAt, &sw.FinishedAt, &sw.SpawnXML,
		&sw.WipeState, &sw.WipeStateAt, &sw.WipeNote, &sw.CharactersCleared)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sw, nil
}

// PendingSwitch is the installation's switch in flight (with its backup and its copy of the map's
// spawn contents), nil when there is none.
func (r *MapRotationRepository) PendingSwitch(ctx context.Context, installationID int64) (*MapRotationSwitch, error) {
	return scanMapSwitch(r.pool.QueryRow(ctx, `SELECT `+mapSwitchColumns+` FROM map_rotation_switches WHERE installation_id=$1 AND status='PENDING'`, installationID))
}

// BeginSwitch records a PENDING switch before anything is written. If one is already in flight it
// is returned instead (created=false) with its attempt count raised: a retry continues a switch,
// it never starts a second one. Backups older than the newest five switches are dropped.
//
// A new switch takes a copy of the map's stored spawn contents in the same statement, and that copy
// is what every attempt of the switch writes: a spawn file uploaded while the switch is in flight
// does not change it. The copy is empty when the map has no contents stored.
func (r *MapRotationRepository) BeginSwitch(ctx context.Context, installationID int64, mapID int64, mapName, mapFile, spawnFile, decidedBy, bootFile string, now time.Time) (*MapRotationSwitch, bool, error) {
	sw, err := scanMapSwitch(r.pool.QueryRow(ctx, `INSERT INTO map_rotation_switches(installation_id, map_id, map_name, map_file, spawn_file, decided_by, boot_file, created_at, spawn_xml)
VALUES($1,$2,$3,$4,$5,$6,$7,$8, (SELECT m.spawn_xml FROM map_rotation_maps m WHERE m.id=$2 AND m.installation_id=$1)) ON CONFLICT (installation_id) WHERE status = 'PENDING' DO NOTHING RETURNING `+mapSwitchColumns,
		installationID, mapID, mapName, mapFile, spawnFile, decidedBy, bootFile, now))
	if err != nil {
		return nil, false, err
	}
	if sw != nil {
		_, _ = r.pool.Exec(ctx, `UPDATE map_rotation_switches SET prev_gameplay=NULL, prev_spawns=NULL
WHERE installation_id=$1 AND status<>'PENDING' AND prev_gameplay IS NOT NULL
  AND id NOT IN (SELECT id FROM map_rotation_switches WHERE installation_id=$1 ORDER BY id DESC LIMIT 5)`, installationID)
		return sw, true, nil
	}
	sw, err = scanMapSwitch(r.pool.QueryRow(ctx, `UPDATE map_rotation_switches SET attempts=attempts+1 WHERE installation_id=$1 AND status='PENDING' RETURNING `+mapSwitchColumns, installationID))
	if err != nil {
		return nil, false, err
	}
	if sw == nil {
		return nil, false, ErrMapSwitchNotPending
	}
	return sw, false, nil
}

// SaveSwitchBackup stores the previous content of the two server files on the PENDING switch,
// once: a backup already saved is never replaced.
func (r *MapRotationRepository) SaveSwitchBackup(ctx context.Context, switchID int64, gameplay, spawns []byte, now time.Time) error {
	tag, err := r.pool.Exec(ctx, `UPDATE map_rotation_switches SET prev_gameplay=$2, prev_spawns=$3, backup_saved_at=$4 WHERE id=$1 AND status='PENDING' AND backup_saved_at IS NULL`,
		switchID, gameplay, spawns, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrMapSwitchNotPending
	}
	return nil
}

// FinishSwitch closes a PENDING switch as APPLIED, FAILED or ROLLED_BACK and ends the period's
// work. halt (non-empty) stops the rotation until the owner saves the settings again; the
// rotation also stops by itself after MapRotationMaxFailures failures in a row. The switch's copy
// of the spawn contents is dropped: it is only needed while the switch is in flight.
func (r *MapRotationRepository) FinishSwitch(ctx context.Context, installationID, switchID int64, status, message, halt string, now time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE map_rotation_switches SET status=$3, message=$4, finished_at=$5, spawn_xml=NULL WHERE id=$2 AND installation_id=$1 AND status='PENDING'`,
		installationID, switchID, status, message, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrMapSwitchNotPending
	}
	if status == MapSwitchApplied {
		_, err = tx.Exec(ctx, `UPDATE map_rotation_settings SET phase='DONE', consecutive_failures=0 WHERE installation_id=$1`, installationID)
	} else {
		_, err = tx.Exec(ctx, `UPDATE map_rotation_settings SET phase='DONE', consecutive_failures=consecutive_failures+1,
    halted_reason = CASE WHEN $2::text <> '' THEN $2::text WHEN consecutive_failures+1 >= $3::int THEN $4::text ELSE halted_reason END WHERE installation_id=$1`,
			installationID, halt, MapRotationMaxFailures, "The map could not be changed several times in a row. Fix the problem, then save the settings to start the rotation again.")
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SwitchAwaitingRestart reports whether a switch was applied whose map is not active yet: the next
// boot loads it.
func (r *MapRotationRepository) SwitchAwaitingRestart(ctx context.Context, installationID int64) (bool, error) {
	var ok bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM map_rotation_switches WHERE installation_id=$1 AND status='APPLIED' AND activated_at IS NULL)`, installationID).Scan(&ok)
	return ok, err
}

// --- fresh characters on a map switch ---------------------------------------------------------------

// SetWipeState stores how far clearing the saved characters got on the PENDING switch, before the
// step the state names is taken. STOP_REQUESTED is only stored on a switch that has not started
// clearing yet, so the server is stopped at most once per switch. DELETED also records that the
// characters were cleared. START_REQUESTED also records the time on the settings: boots seen
// shortly after it belong to this one restart (maprotation.WipeRestartWindow).
func (r *MapRotationRepository) SetWipeState(ctx context.Context, installationID, switchID int64, state string, now time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE map_rotation_switches SET wipe_state=$3, wipe_state_at=$4,
    characters_cleared = CASE WHEN $3::text='DELETED' THEN TRUE WHEN $3::text='STOP_REQUESTED' THEN FALSE ELSE characters_cleared END
WHERE id=$2 AND installation_id=$1 AND status='PENDING'
  AND ($3::text <> 'STOP_REQUESTED' OR wipe_state='NONE')
  AND ($3::text = 'STOP_REQUESTED' OR wipe_state IN ('STOP_REQUESTED','DELETED','START_REQUESTED'))`, installationID, switchID, state, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrMapSwitchNotPending
	}
	if state == MapWipeStartRequested {
		if _, err := tx.Exec(ctx, `UPDATE map_rotation_settings SET wipe_restart_at=$2 WHERE installation_id=$1`, installationID, now); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// CloseWipe records the end of clearing the characters on the PENDING switch: DONE or FAILED, the
// outcome as a sentence and whether the characters were cleared. halt (non-empty) stops the
// rotation in the same transaction: it is set when the server was stopped and could not be seen
// starting again, and a person must look.
func (r *MapRotationRepository) CloseWipe(ctx context.Context, installationID, switchID int64, state, note string, cleared bool, halt string, now time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE map_rotation_switches SET wipe_state=$3, wipe_state_at=$4, wipe_note=$5, characters_cleared=$6
WHERE id=$2 AND installation_id=$1 AND status='PENDING' AND wipe_state NOT IN ('DONE','FAILED')`, installationID, switchID, state, now, note, cleared)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrMapSwitchNotPending
	}
	if halt != "" {
		if _, err := tx.Exec(ctx, `UPDATE map_rotation_settings SET halted_reason=$2 WHERE installation_id=$1`, installationID, halt); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// WipesInProgress lists the installations with a switch whose character clearing was interrupted
// (a crash) and may have left the server stopped. Unlike ActiveInstallations it does not look at
// the owner's switch, a stop of the rotation or a suspension: a server Champion stopped is started
// again whatever happened since.
func (r *MapRotationRepository) WipesInProgress(ctx context.Context) ([]MapRotationTarget, error) {
	rows, err := r.pool.Query(ctx, `
SELECT i.id, i.organization_id, c.guild_id, gs.id, COALESCE(gs.provider_service_id,''), COALESCE(gs.display_name,'')
FROM map_rotation_switches w
JOIN installations i ON i.id = w.installation_id
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
JOIN game_servers gs ON gs.id = i.game_server_id
WHERE w.status='PENDING' AND w.wipe_state IN ('STOP_REQUESTED','DELETED','START_REQUESTED') ORDER BY i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MapRotationTarget
	for rows.Next() {
		var t MapRotationTarget
		if err := rows.Scan(&t.InstallationID, &t.OrganizationID, &t.GuildID, &t.ServerID, &t.NitradoServiceID, &t.ServerName); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetNextRestart stores the next scheduled restart the worker learned (nil = unknown).
func (r *MapRotationRepository) SetNextRestart(ctx context.Context, installationID int64, at *time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE map_rotation_settings SET next_restart_at=$2::timestamptz WHERE installation_id=$1 AND next_restart_at IS DISTINCT FROM $2::timestamptz`, installationID, at)
	return err
}

// MapRotationNotice is one Discord post that is due.
type MapRotationNotice struct {
	Kind   string // VOTE_OPEN, VOTE_RESULT, MAP_CHANGED, SWITCH_FAILED
	Vote   *MapRotationVote
	Switch *MapRotationSwitch
}

const (
	MapNoticeVoteOpen     = "VOTE_OPEN"
	MapNoticeVoteResult   = "VOTE_RESULT"
	MapNoticeMapChanged   = "MAP_CHANGED"
	MapNoticeSwitchFailed = "SWITCH_FAILED"
)

// DueNotices lists the posts not sent yet: an open vote, a recent vote result, a recent map change
// and a recent failed switch. Old ones are never sent late.
func (r *MapRotationRepository) DueNotices(ctx context.Context, installationID int64, now time.Time) ([]MapRotationNotice, error) {
	var out []MapRotationNotice
	open, err := loadMapRotationVote(ctx, r.pool, `installation_id=$1 AND status='OPEN' AND open_announced_at IS NULL AND closes_at > $2`, installationID, now)
	if err != nil {
		return nil, err
	}
	if open != nil {
		out = append(out, MapRotationNotice{Kind: MapNoticeVoteOpen, Vote: open})
	}
	closed, err := loadMapRotationVote(ctx, r.pool, `installation_id=$1 AND status='CLOSED' AND winner_map_id IS NOT NULL AND result_announced_at IS NULL AND closed_at > $2`, installationID, now.Add(-30*time.Minute))
	if err != nil {
		return nil, err
	}
	if closed != nil {
		out = append(out, MapRotationNotice{Kind: MapNoticeVoteResult, Vote: closed})
	}
	rows, err := r.pool.Query(ctx, `SELECT id, map_id, map_name, decided_by, status, message, (activated_at IS NOT NULL AND announced_at IS NULL) AS changed
FROM map_rotation_switches WHERE installation_id=$1 AND (
    (status='APPLIED' AND activated_at IS NOT NULL AND announced_at IS NULL AND activated_at > $2)
 OR (status IN ('FAILED','ROLLED_BACK') AND alerted_at IS NULL AND finished_at > $3)) ORDER BY id`, installationID, now.Add(-time.Hour), now.Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// A map change (or a failure) is reported before the vote that follows it.
	votes := out
	out = nil
	for rows.Next() {
		var sw MapRotationSwitch
		var changed bool
		if err := rows.Scan(&sw.ID, &sw.MapID, &sw.MapName, &sw.DecidedBy, &sw.Status, &sw.Message, &changed); err != nil {
			return nil, err
		}
		kind := MapNoticeSwitchFailed
		if changed && sw.Status == MapSwitchApplied {
			kind = MapNoticeMapChanged
		}
		out = append(out, MapRotationNotice{Kind: kind, Switch: &sw})
	}
	return append(out, votes...), rows.Err()
}

// MarkNotice records that a notice was sent (or deliberately skipped), so it is never sent twice.
func (r *MapRotationRepository) MarkNotice(ctx context.Context, n MapRotationNotice, now time.Time) error {
	var err error
	switch n.Kind {
	case MapNoticeVoteOpen:
		_, err = r.pool.Exec(ctx, `UPDATE map_rotation_votes SET open_announced_at=$2 WHERE id=$1 AND open_announced_at IS NULL`, n.Vote.ID, now)
	case MapNoticeVoteResult:
		_, err = r.pool.Exec(ctx, `UPDATE map_rotation_votes SET result_announced_at=$2 WHERE id=$1 AND result_announced_at IS NULL`, n.Vote.ID, now)
	case MapNoticeMapChanged:
		_, err = r.pool.Exec(ctx, `UPDATE map_rotation_switches SET announced_at=$2 WHERE id=$1 AND announced_at IS NULL`, n.Switch.ID, now)
	case MapNoticeSwitchFailed:
		_, err = r.pool.Exec(ctx, `UPDATE map_rotation_switches SET alerted_at=$2 WHERE id=$1 AND alerted_at IS NULL`, n.Switch.ID, now)
	}
	return err
}
