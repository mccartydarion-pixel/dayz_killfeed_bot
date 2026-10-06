package database

// StadiumSQL is the Stadium (docs/STADIUM.md): one tournament arena per installation, built from
// DayZ map objects and written as an object-spawner file into the server's custom/ folder. The row
// holds the owner's configuration (params), whether the file is on the server (status, the file's
// digest and object count, when it was built or emptied) and the last build or remove outcome.
// Nothing here is read by any existing feature. Additive.
const StadiumSQL = `
CREATE TABLE IF NOT EXISTS stadiums (
    installation_id BIGINT PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    params JSONB NOT NULL,
    status TEXT NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT','BUILT','REMOVED')),
    file_sha256 TEXT CHECK (file_sha256 IS NULL OR file_sha256 ~ '^[0-9a-f]{64}$'),
    object_count INTEGER NOT NULL DEFAULT 0 CHECK (object_count >= 0),
    built_at TIMESTAMPTZ,
    removed_at TIMESTAMPTZ,
    last_outcome JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE
);
`
