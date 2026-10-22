package database

// migrations0126onward holds migrations 0126 and later, in execution order. It is one fragment of
// the registry assembled in migrations.go; never edit an applied migration.
var migrations0126onward = []Migration{
	{
		// Interactive reads (interactive_reads_schema.go): a partial index for "who is online on
		// this server". One small index on a small table; additive.
		Name: "0126_interactive_read_indexes",
		SQL:  InteractiveReadIndexesSQL,
	},
	{
		// Map rotation: fresh characters on every map switch (docs/MAP_ROTATION.md). One owner
		// option (default off), the time of Champion's own restart, and the progress and outcome of
		// clearing the characters on a switch. Additive.
		Name: "0127_map_rotation_wipe_characters",
		SQL:  MapRotationWipeCharactersSQL,
	},
	{
		// Map rotation: a map's picture is uploaded and stored in Champion instead of linked
		// (docs/MAP_ROTATION.md). Three nullable columns on map_rotation_maps. Additive.
		Name: "0128_map_rotation_map_images",
		SQL:  MapRotationMapImagesSQL,
	},
	{
		// Ranked seasons: the wait before the same attacker earns RP from the same victim again
		// becomes part of a season's frozen rules (docs/RANKED_SERVER_SEASONS.md). Existing
		// seasons, active and archived, keep the old fixed five minutes. Additive.
		Name: "0129_ranked_same_victim_cooldown",
		SQL:  RankedSameVictimCooldownSQL,
	},
	{
		// Ranked seasons: the same-victim wait can change during a season
		// (docs/RANKED_SERVER_SEASONS.md). The history of values lets the award path use the wait
		// that was in force when a kill happened. Existing seasons are seeded from their own row,
		// effective from the season start. Additive.
		Name: "0130_ranked_cooldown_changes",
		SQL:  RankedCooldownChangesSQL,
	},
	{
		// The Stadium (docs/STADIUM.md): the owner's arena configuration and its build state, one
		// row per installation. Additive.
		Name: "0131_stadiums",
		SQL:  StadiumSQL,
	},
	{
		// Tournament mode (docs/TOURNAMENTS.md): tournaments, entries, the bracket's matches,
		// rounds, the champion's title and the prize payouts. Additive.
		Name: "0132_tournaments",
		SQL:  TournamentSQL,
	},
	{
		// Self-serve organization erase (docs/SAAS_HTTP_API.md "Erase an organization"): payment
		// records and the one-trial-per-account marker outlive the organization they belonged to.
		// Both references become nullable ON DELETE SET NULL instead of CASCADE. No data changes.
		Name: "0133_erase_keeps_billing_records",
		SQL:  EraseKeepsBillingRecordsSQL,
	},
	{
		// Billing pause (docs/BILLING.md "Payment failure handling"): when a subscription first
		// became PAST_DUE, so the grace period is measured from the first failed payment and not
		// from updated_at or current_period_start, which Stripe moves on every dunning retry.
		Name: "0134_subscription_past_due_since",
		SQL:  SubscriptionPastDueSinceSQL,
	},
	{
		// Faction Hub membership in the shape of the old Discord faction_members table, for the
		// base and security features that still asked that table (nothing writes it any more).
		Name: "0135_live_faction_members_view",
		SQL:  LiveFactionMembersViewSQL,
	},
	{
		// An installation can move to another game server: per-server C.A.S.E., base and
		// security rows stay with the server they were recorded for instead of blocking the move.
		Name: "0136_installation_server_switch",
		SQL:  InstallationServerSwitchSQL,
	},
	{
		// Zone FACTION entries now name Faction Hub factions; old entries named old-system ids.
		Name: "0137_zone_faction_entries_hub",
		SQL:  ZoneFactionEntriesHubSQL,
	},
	{
		// Base faction grants name a Faction Hub faction of the base's installation.
		Name: "0138_base_grant_hub_faction",
		SQL:  BaseGrantHubFactionSQL,
	},
	{
		// Kills and season results record Faction Hub factions.
		Name: "0139_kill_hub_factions",
		SQL:  KillHubFactionsSQL,
	},
	{
		// FACTION events score and rank Faction Hub factions.
		Name: "0140_event_hub_factions",
		SQL:  EventHubFactionsSQL,
	},
}

// SubscriptionPastDueSinceSQL adds subscriptions.past_due_since: set when the status becomes
// PAST_DUE, kept while it stays PAST_DUE, cleared by any other status (ApplyProviderState). Rows
// already PAST_DUE start their grace now.
const SubscriptionPastDueSinceSQL = `
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS past_due_since TIMESTAMPTZ;
UPDATE subscriptions SET past_due_since = NOW() WHERE status = 'PAST_DUE' AND past_due_since IS NULL;
`

// EraseKeepsBillingRecordsSQL keeps billing_transactions (needed for tax and accounting) and
// trial_grants (one no-card trial per Discord account) when their organization is deleted: the
// organization_id is cleared instead of the row being removed.
const EraseKeepsBillingRecordsSQL = `
ALTER TABLE billing_transactions ALTER COLUMN organization_id DROP NOT NULL;
ALTER TABLE billing_transactions DROP CONSTRAINT IF EXISTS billing_transactions_organization_id_fkey;
ALTER TABLE billing_transactions ADD CONSTRAINT billing_transactions_organization_id_fkey
    FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE SET NULL;
ALTER TABLE trial_grants ALTER COLUMN organization_id DROP NOT NULL;
ALTER TABLE trial_grants DROP CONSTRAINT IF EXISTS trial_grants_organization_id_fkey;
ALTER TABLE trial_grants ADD CONSTRAINT trial_grants_organization_id_fkey
    FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE SET NULL;
`

// LiveFactionMembersViewSQL is live_faction_members: one row per Faction Hub member whose Discord
// account has a VERIFIED link to a DayZ player on the installation's guild, as (guild_id,
// installation_id, faction_id, player_id, role, active). faction_id is the hub faction; role is
// LEADER, OFFICER or MEMBER; active is always TRUE (leaving deletes the hub row).
const LiveFactionMembersViewSQL = `
CREATE OR REPLACE VIEW live_faction_members AS
SELECT c.guild_id, m.installation_id, m.faction_id, l.player_id, m.role_key AS role, TRUE AS active
FROM hub_faction_members m
JOIN installations i ON i.id = m.installation_id
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
JOIN app_users u ON u.id = m.user_id
JOIN player_links l ON l.discord_user_id = u.discord_user_id AND l.guild_id = c.guild_id AND l.status = 'VERIFIED';
`

// InstallationServerSwitchSQL lets an installation change game servers. Seventeen per-server
// tables (C.A.S.E. settings, cases, verdicts, alerts, bases and their requests, rent, Black Box,
// raid alarm, Perimeter Watch, Faction Security and the security store) referenced
// installations(id, game_server_id) together, so once any of them had a row, pointing the
// installation at another server failed on the foreign key and the server switch answered 500.
//
// Each such key becomes a plain installation key, and the guard it gave on writes stays as a
// trigger: a row is written (or re-pointed) only for the installation's current server (and, for
// C.A.S.E. cases, its current Discord connection), still as a foreign_key_violation. Rows already
// stored are not checked again, so they stay with the server they were recorded for: queries
// scoped to the installation's current server no longer see them, and switching back shows them
// again. Constraints are found in the catalog because one of them was created without a name.
const InstallationServerSwitchSQL = `
CREATE OR REPLACE FUNCTION installation_current_server_guard() RETURNS trigger AS $$
DECLARE conn TEXT;
BEGIN
  IF TG_OP = 'UPDATE' AND NEW.installation_id IS NOT DISTINCT FROM OLD.installation_id
     AND NEW.server_id IS NOT DISTINCT FROM OLD.server_id THEN
    RETURN NEW;
  END IF;
  conn := to_jsonb(NEW) ->> 'discord_guild_connection_id';
  IF NOT EXISTS (SELECT 1 FROM installations i
                 WHERE i.id = NEW.installation_id AND i.game_server_id = NEW.server_id
                   AND (conn IS NULL OR i.discord_guild_connection_id = conn::bigint)) THEN
    RAISE EXCEPTION 'server % is not the current server of installation %', NEW.server_id, NEW.installation_id
      USING ERRCODE = 'foreign_key_violation';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;

DO $$
DECLARE r RECORD;
BEGIN
  FOR r IN
    SELECT c.conname, c.conrelid::regclass AS tbl, c.conrelid::regclass::text AS tblname
    FROM pg_constraint c
    WHERE c.contype = 'f' AND c.confrelid = 'installations'::regclass
      AND EXISTS (SELECT 1 FROM unnest(c.confkey) AS k(attnum)
                  JOIN pg_attribute a ON a.attrelid = c.confrelid AND a.attnum = k.attnum
                  WHERE a.attname = 'game_server_id')
  LOOP
    EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', r.tbl, r.conname);
    EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I FOREIGN KEY (installation_id) REFERENCES installations(id) ON DELETE CASCADE',
      r.tbl, r.tblname || '_installation_fkey');
    EXECUTE format('CREATE TRIGGER %I BEFORE INSERT OR UPDATE OF installation_id, server_id ON %s FOR EACH ROW EXECUTE FUNCTION installation_current_server_guard()',
      r.tblname || '_current_server', r.tbl);
  END LOOP;
END $$;
`

// ZoneFactionEntriesHubSQL clears the zone ignore and authorized-list FACTION entries written for
// the old Discord faction system. From now on a FACTION entry names a Faction Hub faction of the
// zone's installation (checked when it is added). The old entries named ids of the retired
// factions table, which nothing has written for a long time, so they matched no one; read as Hub
// ids they would match whichever unrelated Hub faction happens to share the number.
const ZoneFactionEntriesHubSQL = `
DELETE FROM zone_ignore_entries WHERE entry_type = 'FACTION';
DELETE FROM zone_authorized_entries WHERE entry_type = 'FACTION';
`

// BaseGrantHubFactionSQL lets a base grant name a Faction Hub faction: case_base_authorizations
// gets hub_faction_id, keyed to hub_factions(id, installation_id) so a grant can only name a
// faction of the base's own installation (dissolving the faction removes its grants). A grant
// names exactly one subject: a player, a Hub faction, or (rows written before this change) a
// faction of the retired Discord system, whose column and foreign key stay for those rows. The
// grant trigger's immutability and overlap rules now cover the new column too.
const BaseGrantHubFactionSQL = `
ALTER TABLE case_base_authorizations ADD COLUMN IF NOT EXISTS hub_faction_id BIGINT;
ALTER TABLE case_base_authorizations ADD CONSTRAINT fk_case_base_grant_hub_faction
 FOREIGN KEY (hub_faction_id, installation_id) REFERENCES hub_factions(id, installation_id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS idx_case_base_grants_hub_faction
 ON case_base_authorizations(hub_faction_id) WHERE hub_faction_id IS NOT NULL;

-- The one-subject check was created without a name.
DO $$
DECLARE r RECORD;
BEGIN
 FOR r IN SELECT conname FROM pg_constraint
  WHERE conrelid = 'case_base_authorizations'::regclass AND contype = 'c'
   AND pg_get_constraintdef(oid) LIKE '%player_id IS NOT NULL%<>%faction_id IS NOT NULL%'
 LOOP
  EXECUTE format('ALTER TABLE case_base_authorizations DROP CONSTRAINT %I', r.conname);
 END LOOP;
END $$;
ALTER TABLE case_base_authorizations ADD CONSTRAINT ck_case_base_grant_one_subject
 CHECK (num_nonnulls(player_id, faction_id, hub_faction_id) = 1);

CREATE OR REPLACE FUNCTION case_base_grant_requires_draft()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' THEN
  IF ROW(NEW.installation_id,NEW.guild_id,NEW.server_id,NEW.base_id,
         NEW.player_id,NEW.faction_id,NEW.hub_faction_id,NEW.valid_from,NEW.created_at)
     IS DISTINCT FROM
     ROW(OLD.installation_id,OLD.guild_id,OLD.server_id,OLD.base_id,
         OLD.player_id,OLD.faction_id,OLD.hub_faction_id,OLD.valid_from,OLD.created_at) THEN
   RAISE EXCEPTION 'C.A.S.E. base grant identity and start are immutable'
    USING ERRCODE='23514';
  END IF;
  IF OLD.valid_until IS NOT NULL AND
     (NEW.valid_until IS NULL OR NEW.valid_until>OLD.valid_until) THEN
   RAISE EXCEPTION 'C.A.S.E. base grant end cannot be reopened or extended'
    USING ERRCODE='23514';
  END IF;
 END IF;
 PERFORM 1 FROM case_registered_bases b
 WHERE b.installation_id=NEW.installation_id AND b.guild_id=NEW.guild_id
   AND b.server_id=NEW.server_id AND b.id=NEW.base_id AND b.state='DRAFT'
 FOR UPDATE;
 IF NOT FOUND THEN
  RAISE EXCEPTION 'C.A.S.E. base draft unavailable for grant'
   USING ERRCODE='23514';
 END IF;
 -- Intervals are half-open. The base row lock serializes concurrent grants
 -- for the same claim; no two intervals for one subject may overlap.
 PERFORM 1 FROM case_base_authorizations a
 WHERE a.installation_id=NEW.installation_id AND a.guild_id=NEW.guild_id
   AND a.server_id=NEW.server_id AND a.base_id=NEW.base_id
   AND a.id<>NEW.id
   AND ((NEW.player_id IS NOT NULL AND a.player_id=NEW.player_id)
     OR (NEW.faction_id IS NOT NULL AND a.faction_id=NEW.faction_id)
     OR (NEW.hub_faction_id IS NOT NULL AND a.hub_faction_id=NEW.hub_faction_id))
   AND (a.valid_until IS NULL OR NEW.valid_from<a.valid_until)
   AND (NEW.valid_until IS NULL OR a.valid_from<NEW.valid_until);
 IF FOUND THEN
  RAISE EXCEPTION 'C.A.S.E. base grant overlaps an existing subject interval'
   USING ERRCODE='23P01';
 END IF;
 RETURN NEW;
END;
$$;
`

// KillHubFactionsSQL records Faction Hub factions on kills and season results. The old
// kills.killer_faction_id / victim_faction_id and season_results.top_faction_id reference the
// retired Discord factions table, whose ids are not Hub ids, so new columns hold the Hub ids:
//   - kills.killer_hub_faction_id / victim_hub_faction_id: a snapshot of each player's faction at
//     kill time. No foreign key and no index: kills is the largest table, adding a nullable column
//     without them is a catalog change only, and a dissolved faction must not rewrite history.
//   - season_results.top_hub_faction_id and top_faction_label ("[TAG] Name" as it was when the
//     season ended, so the season card still names a faction dissolved later).
const KillHubFactionsSQL = `
ALTER TABLE kills ADD COLUMN IF NOT EXISTS killer_hub_faction_id BIGINT;
ALTER TABLE kills ADD COLUMN IF NOT EXISTS victim_hub_faction_id BIGINT;
ALTER TABLE season_results ADD COLUMN IF NOT EXISTS top_hub_faction_id BIGINT REFERENCES hub_factions(id) ON DELETE SET NULL;
ALTER TABLE season_results ADD COLUMN IF NOT EXISTS top_faction_label TEXT;
`

// EventHubFactionsSQL lets FACTION events score Faction Hub factions. event_scores gets
// hub_faction_id (with its own unique key per event) and a score row names exactly one subject; an
// event's winner gets event_results.winner_hub_faction_id; and a faction's placement goes to
// hub_faction_event_results with its "[TAG] Name" kept, so a faction dissolved later still shows
// (faction_event_results is keyed by a faction of the retired Discord system and cannot hold Hub
// ids). Rows naming old-system factions stay as they are.
const EventHubFactionsSQL = `
ALTER TABLE event_scores ADD COLUMN IF NOT EXISTS hub_faction_id BIGINT REFERENCES hub_factions(id) ON DELETE CASCADE;
CREATE UNIQUE INDEX IF NOT EXISTS uq_event_hub_faction_score ON event_scores(event_id,hub_faction_id) WHERE hub_faction_id IS NOT NULL;
-- The one-subject check was created without a name.
DO $$
DECLARE r RECORD;
BEGIN
 FOR r IN SELECT conname FROM pg_constraint
  WHERE conrelid = 'event_scores'::regclass AND contype = 'c'
   AND pg_get_constraintdef(oid) LIKE '%player_id IS NOT NULL%<>%faction_id IS NOT NULL%'
 LOOP
  EXECUTE format('ALTER TABLE event_scores DROP CONSTRAINT %I', r.conname);
 END LOOP;
END $$;
ALTER TABLE event_scores ADD CONSTRAINT ck_event_scores_one_subject
 CHECK (num_nonnulls(player_id, faction_id, hub_faction_id) = 1);

ALTER TABLE event_results ADD COLUMN IF NOT EXISTS winner_hub_faction_id BIGINT REFERENCES hub_factions(id) ON DELETE SET NULL;

CREATE TABLE IF NOT EXISTS hub_faction_event_results (
 event_id BIGINT NOT NULL REFERENCES competitive_events(id) ON DELETE CASCADE,
 hub_faction_id BIGINT REFERENCES hub_factions(id) ON DELETE SET NULL,
 faction_label TEXT NOT NULL,
 placement INTEGER NOT NULL,
 score DOUBLE PRECISION NOT NULL,
 reward_points INTEGER NOT NULL DEFAULT 0,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY (event_id, placement)
);
`
