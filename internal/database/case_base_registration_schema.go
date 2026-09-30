package database

// CASEBaseRegistrationSQL is inert registration storage for Core Eight Base
// Boost. Nothing reads it to make a detector conclusion or case. The owner-enabled
// Base Raid Alarm (base_raid_alarm_schema.go) reads it to DM a base's owner.
// Composite keys prevent a base or grant from crossing its installation,
// guild, game server, or player/faction boundary.
const CASEBaseRegistrationSQL = `
CREATE UNIQUE INDEX IF NOT EXISTS uq_case_game_server_scope
 ON game_servers(guild_id,id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_case_player_scope
 ON players(guild_id,id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_case_faction_scope
 ON factions(guild_id,id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_case_installation_server
 ON installations(id,game_server_id);

CREATE TABLE IF NOT EXISTS case_registered_bases (
 id BIGSERIAL PRIMARY KEY,
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 owner_player_id BIGINT NOT NULL,
 map_key TEXT NOT NULL CHECK (char_length(map_key) BETWEEN 1 AND 80),
 name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 128),
 center_x DOUBLE PRECISION NOT NULL CHECK (center_x BETWEEN -100000 AND 100000),
 center_z DOUBLE PRECISION NOT NULL CHECK (center_z BETWEEN -100000 AND 100000),
 radius DOUBLE PRECISION NOT NULL CHECK (radius BETWEEN 1 AND 5000),
 state TEXT NOT NULL DEFAULT 'DRAFT' CHECK (state IN ('DRAFT','REVIEWED','REVOKED')),
 reviewed_at TIMESTAMPTZ,
 revoked_at TIMESTAMPTZ,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CHECK ((state='REVIEWED' AND reviewed_at IS NOT NULL AND revoked_at IS NULL)
     OR (state='REVOKED' AND revoked_at IS NOT NULL)
     OR (state='DRAFT' AND reviewed_at IS NULL AND revoked_at IS NULL)),
 CONSTRAINT fk_case_base_installation_server FOREIGN KEY (installation_id,server_id)
  REFERENCES installations(id,game_server_id) ON DELETE CASCADE,
 CONSTRAINT fk_case_base_game_server FOREIGN KEY (guild_id,server_id)
  REFERENCES game_servers(guild_id,id) ON DELETE CASCADE,
 CONSTRAINT fk_case_base_owner FOREIGN KEY (guild_id,owner_player_id)
  REFERENCES players(guild_id,id) ON DELETE RESTRICT,
 CONSTRAINT uq_case_base_scope UNIQUE (installation_id,guild_id,server_id,id)
);
CREATE INDEX IF NOT EXISTS idx_case_bases_scope
 ON case_registered_bases(installation_id,guild_id,server_id,state,id);

CREATE TABLE IF NOT EXISTS case_base_authorizations (
 id BIGSERIAL PRIMARY KEY,
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 base_id BIGINT NOT NULL,
 player_id BIGINT,
 faction_id BIGINT,
 valid_from TIMESTAMPTZ NOT NULL,
 valid_until TIMESTAMPTZ,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CHECK ((player_id IS NOT NULL) <> (faction_id IS NOT NULL)),
 CHECK (valid_until IS NULL OR valid_until > valid_from),
 CONSTRAINT fk_case_base_grant_scope FOREIGN KEY (installation_id,guild_id,server_id,base_id)
  REFERENCES case_registered_bases(installation_id,guild_id,server_id,id) ON DELETE CASCADE,
 CONSTRAINT fk_case_base_grant_player FOREIGN KEY (guild_id,player_id)
  REFERENCES players(guild_id,id) ON DELETE RESTRICT,
 CONSTRAINT fk_case_base_grant_faction FOREIGN KEY (guild_id,faction_id)
  REFERENCES factions(guild_id,id) ON DELETE RESTRICT
);

-- Lock the base claim while writing a grant. This serializes grant changes
-- with withdrawal and keeps direct database writers under the same rule.
CREATE OR REPLACE FUNCTION case_base_grant_requires_draft()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' THEN
  IF ROW(NEW.installation_id,NEW.guild_id,NEW.server_id,NEW.base_id,
         NEW.player_id,NEW.faction_id,NEW.valid_from,NEW.created_at)
     IS DISTINCT FROM
     ROW(OLD.installation_id,OLD.guild_id,OLD.server_id,OLD.base_id,
         OLD.player_id,OLD.faction_id,OLD.valid_from,OLD.created_at) THEN
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
     OR (NEW.faction_id IS NOT NULL AND a.faction_id=NEW.faction_id))
   AND (a.valid_until IS NULL OR NEW.valid_from<a.valid_until)
   AND (NEW.valid_until IS NULL OR a.valid_from<NEW.valid_until);
 IF FOUND THEN
  RAISE EXCEPTION 'C.A.S.E. base grant overlaps an existing subject interval'
   USING ERRCODE='23P01';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER trg_case_base_grant_requires_draft
 BEFORE INSERT OR UPDATE ON case_base_authorizations
 FOR EACH ROW EXECUTE FUNCTION case_base_grant_requires_draft();
CREATE INDEX IF NOT EXISTS idx_case_base_grants_scope
 ON case_base_authorizations(installation_id,guild_id,server_id,base_id,valid_from);
`;
