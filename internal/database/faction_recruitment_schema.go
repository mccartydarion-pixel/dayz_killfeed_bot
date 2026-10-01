package database

// FactionRecruitmentSQL records the one recruitment card a faction may have in the
// FACTION_RECRUITMENT channel (docs/FACTIONS.md "Recruitment"). One row per faction; the row goes
// with the faction (cascade) so a dissolved faction never keeps a card. Additive only.
const FactionRecruitmentSQL = `
CREATE TABLE IF NOT EXISTS hub_faction_recruit_posts (
    faction_id BIGINT PRIMARY KEY,
    installation_id BIGINT NOT NULL,
    channel_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    posted_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    posted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (faction_id, installation_id) REFERENCES hub_factions(id, installation_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_hub_faction_recruit_posts_installation ON hub_faction_recruit_posts(installation_id);
`
