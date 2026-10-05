package database

// PlatformStaffSQL stores Champion's platform staff: people who may open the Owner Hub and read
// everything in it, and change nothing (docs/ADMIN_API.md "Roles"). The platform owner manages
// the list from the Owner Hub. Platform owners themselves are never stored here: they come only
// from CHAMPION_ADMIN_DISCORD_IDS. Additive only.
const PlatformStaffSQL = `
CREATE TABLE IF NOT EXISTS platform_staff (
    discord_user_id TEXT PRIMARY KEY CHECK (discord_user_id ~ '^[0-9]{15,20}$'),
    note TEXT NOT NULL DEFAULT '' CHECK (char_length(note) <= 120),
    added_by TEXT NOT NULL DEFAULT '',
    added_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`
