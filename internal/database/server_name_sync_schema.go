package database

// ServerNameSyncSQL lets a game server's name follow its Nitrado name unless the owner typed one in
// Champion (docs/SERVER_NAME_SYNC.md).
//
//   - display_name_custom: the owner set display_name in Champion; the sync never overwrites it.
//   - provider_display_name: the last name read from Nitrado (NULL until the first read).
//
// Servers that exist already: a rename in Champion is recorded in admin_audit_log as
// SERVER_NAME_EDIT with the new name. A server whose current name is the name of such a rename is
// marked custom; every other existing name came from Nitrado when the server was connected and
// keeps following it. Additive.
const ServerNameSyncSQL = `
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS display_name_custom BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS provider_display_name TEXT;

UPDATE game_servers gs SET display_name_custom = TRUE
WHERE COALESCE(gs.display_name, '') <> ''
  AND EXISTS (
    SELECT 1 FROM admin_audit_log l
    JOIN installations i ON i.id = l.installation_id
    WHERE i.game_server_id = gs.id
      AND l.action = 'SERVER_NAME_EDIT'
      AND l.result = 'success'
      AND l.after_state->>'name' = gs.display_name
  );
`
