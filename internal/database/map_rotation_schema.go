package database

// MapRotationSQL is map rotation with a player vote (docs/MAP_ROTATION.md): the owner's settings
// and Champion's rotation state per installation, the configured maps, votes with their options
// and ballots, and the log of every switch (with the backup of the two server files a switch
// writes). Everything is off until an owner turns it on (enabled defaults to FALSE) and nothing
// here is read by any existing feature. Additive.
const MapRotationSQL = `
CREATE TABLE IF NOT EXISTS map_rotation_settings (
    installation_id BIGINT PRIMARY KEY REFERENCES installations(id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    every_restarts INTEGER NOT NULL DEFAULT 1 CHECK (every_restarts BETWEEN 1 AND 3),
    rotation_order TEXT NOT NULL DEFAULT 'SEQUENCE' CHECK (rotation_order IN ('SEQUENCE','RANDOM')),
    vote_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    vote_minutes INTEGER NOT NULL DEFAULT 30 CHECK (vote_minutes BETWEEN 5 AND 120),
    ping_everyone BOOLEAN NOT NULL DEFAULT TRUE,
    announce_channel_id TEXT,
    -- Rotation state, written by the worker.
    current_map_id BIGINT,
    current_map_name TEXT,
    current_map_file TEXT,
    current_since TIMESTAMPTZ,
    next_map_id BIGINT,
    next_decided_by TEXT CHECK (next_decided_by IN ('ROTATION','VOTE','STAFF')),
    staff_next_map_id BIGINT,
    last_boot_file TEXT NOT NULL DEFAULT '',
    restarts_since_switch INTEGER NOT NULL DEFAULT 0,
    phase TEXT NOT NULL DEFAULT 'IDLE' CHECK (phase IN ('IDLE','VOTING','DECIDED','DONE')),
    phase_started_at TIMESTAMPTZ,
    next_restart_at TIMESTAMPTZ,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    halted_reason TEXT NOT NULL DEFAULT '',
    lease_owner TEXT,
    lease_until TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS map_rotation_maps (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    map_file TEXT NOT NULL,
    spawn_file TEXT NOT NULL,
    image_url TEXT,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    position INTEGER NOT NULL,
    map_file_found BOOLEAN,
    spawn_file_found BOOLEAN,
    checked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_map_rotation_maps_installation ON map_rotation_maps(installation_id, position);

CREATE TABLE IF NOT EXISTS map_rotation_votes (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('OPEN','CLOSED')),
    opens_at TIMESTAMPTZ NOT NULL,
    closes_at TIMESTAMPTZ NOT NULL,
    closed_at TIMESTAMPTZ,
    winner_map_id BIGINT,
    winner_name TEXT NOT NULL DEFAULT '',
    decided_by TEXT CHECK (decided_by IN ('ROTATION','VOTE','STAFF')),
    boot_file TEXT NOT NULL DEFAULT '',
    open_announced_at TIMESTAMPTZ,
    result_announced_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- At most one open vote per installation.
CREATE UNIQUE INDEX IF NOT EXISTS uq_map_rotation_votes_open ON map_rotation_votes(installation_id) WHERE status = 'OPEN';
CREATE INDEX IF NOT EXISTS idx_map_rotation_votes_installation ON map_rotation_votes(installation_id, id DESC);

-- The options are a snapshot taken when the vote opens, so a later change to the maps cannot
-- change what players were offered.
CREATE TABLE IF NOT EXISTS map_rotation_vote_options (
    vote_id BIGINT NOT NULL REFERENCES map_rotation_votes(id) ON DELETE CASCADE,
    map_id BIGINT NOT NULL,
    name TEXT NOT NULL,
    image_url TEXT,
    position INTEGER NOT NULL,
    PRIMARY KEY (vote_id, map_id)
);

-- One ballot per linked player per vote; voting again replaces it.
CREATE TABLE IF NOT EXISTS map_rotation_ballots (
    vote_id BIGINT NOT NULL REFERENCES map_rotation_votes(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    map_id BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (vote_id, player_id)
);

-- Every switch. prev_gameplay / prev_spawns are the backup of cfggameplay.json and
-- cfgplayerspawnpoints.xml, saved before the first write (kept for the newest switches only).
CREATE TABLE IF NOT EXISTS map_rotation_switches (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    map_id BIGINT,
    map_name TEXT NOT NULL,
    map_file TEXT NOT NULL,
    spawn_file TEXT NOT NULL,
    decided_by TEXT NOT NULL CHECK (decided_by IN ('ROTATION','VOTE','STAFF')),
    status TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','APPLIED','FAILED','ROLLED_BACK')),
    message TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 1,
    prev_gameplay BYTEA,
    prev_spawns BYTEA,
    backup_saved_at TIMESTAMPTZ,
    boot_file TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ,
    activated_at TIMESTAMPTZ,
    announced_at TIMESTAMPTZ,
    alerted_at TIMESTAMPTZ
);
-- At most one switch in flight per installation: a retry continues it, never starts a second.
CREATE UNIQUE INDEX IF NOT EXISTS uq_map_rotation_switches_pending ON map_rotation_switches(installation_id) WHERE status = 'PENDING';
CREATE INDEX IF NOT EXISTS idx_map_rotation_switches_installation ON map_rotation_switches(installation_id, id DESC);
`

// MapRotationSpawnUploadSQL moves a map's spawn points from a file in the server's custom folder to
// contents the owner uploads on the website (docs/MAP_ROTATION.md). map_rotation_maps.spawn_xml is
// the uploaded file (spawn_file stays, as the uploaded file's name); map_rotation_switches.spawn_xml
// is the copy a switch takes when it begins, so an interrupted switch finishes with the contents it
// started with. A map saved before this migration has no contents until its owner uploads them.
// Additive.
const MapRotationSpawnUploadSQL = `
ALTER TABLE map_rotation_maps ADD COLUMN IF NOT EXISTS spawn_xml BYTEA;
ALTER TABLE map_rotation_switches ADD COLUMN IF NOT EXISTS spawn_xml BYTEA;
`

// MapRotationWipeCharactersSQL adds "fresh characters on every map switch" (docs/MAP_ROTATION.md).
// map_rotation_settings.wipe_characters is the owner's option (off unless switched on) and
// wipe_restart_at the time Champion last started a server again after clearing characters, which
// the restart count uses. On a switch, wipe_state is how far clearing the characters got (NONE,
// STOP_REQUESTED, DELETED, START_REQUESTED, DONE, FAILED) with its time, wipe_note the outcome in
// plain language and characters_cleared whether the saved characters were removed (NULL: not
// attempted). Additive.
const MapRotationWipeCharactersSQL = `
ALTER TABLE map_rotation_settings ADD COLUMN IF NOT EXISTS wipe_characters BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE map_rotation_settings ADD COLUMN IF NOT EXISTS wipe_restart_at TIMESTAMPTZ;
ALTER TABLE map_rotation_switches ADD COLUMN IF NOT EXISTS wipe_state TEXT NOT NULL DEFAULT 'NONE';
ALTER TABLE map_rotation_switches ADD COLUMN IF NOT EXISTS wipe_state_at TIMESTAMPTZ;
ALTER TABLE map_rotation_switches ADD COLUMN IF NOT EXISTS wipe_note TEXT NOT NULL DEFAULT '';
ALTER TABLE map_rotation_switches ADD COLUMN IF NOT EXISTS characters_cleared BOOLEAN;
`

// MapRotationMapImagesSQL stores a map's picture in Champion (docs/MAP_ROTATION.md): the owner
// uploads it on the website and the website serves it from its own address, because a pasted link
// (image_url, which stays) stops loading when its host expires it or is blocked. image_data is the
// picture, image_type its content type (image/jpeg, image/png or image/webp) and image_version the
// first 16 hex characters of the SHA-256 of the bytes. All NULL when no picture is stored. Additive.
const MapRotationMapImagesSQL = `
ALTER TABLE map_rotation_maps ADD COLUMN IF NOT EXISTS image_data BYTEA;
ALTER TABLE map_rotation_maps ADD COLUMN IF NOT EXISTS image_type TEXT;
ALTER TABLE map_rotation_maps ADD COLUMN IF NOT EXISTS image_version TEXT;
`
