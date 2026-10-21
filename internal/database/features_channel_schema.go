package database

// GuildFeaturesChannelSQL records the channel /features created (docs/FEATURES_CHANNEL.md), so the
// next /features removes exactly that channel and nothing else. Additive: NULL means no channel.
const GuildFeaturesChannelSQL = `
ALTER TABLE guilds ADD COLUMN IF NOT EXISTS features_channel_id TEXT;
`
