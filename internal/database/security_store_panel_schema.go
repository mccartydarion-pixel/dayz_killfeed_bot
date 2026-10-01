package database

// SecurityStorePanelSQL stores where the server owner wants the Security
// Store panel posted in Discord, and the message the bot keeps updating there.
const SecurityStorePanelSQL = `
CREATE TABLE IF NOT EXISTS security_store_panels (
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 channel_id TEXT NOT NULL CHECK (channel_id ~ '^[0-9]{5,25}$'),
 message_id TEXT NOT NULL DEFAULT '' CHECK (message_id = '' OR message_id ~ '^[0-9]{5,25}$'),
 enabled BOOLEAN NOT NULL DEFAULT TRUE,
 last_posted_at TIMESTAMPTZ,
 last_error TEXT NOT NULL DEFAULT '' CHECK (char_length(last_error) <= 300),
 updated_by_user_id BIGINT,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY (installation_id,server_id),
 CONSTRAINT fk_security_panel_installation_server FOREIGN KEY (installation_id,server_id)
  REFERENCES installations(id,game_server_id) ON DELETE CASCADE,
 CONSTRAINT fk_security_panel_guild_server FOREIGN KEY (guild_id,server_id)
  REFERENCES game_servers(guild_id,id) ON DELETE CASCADE
);
`
