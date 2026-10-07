package database

// TournamentSQL is tournament mode (docs/TOURNAMENTS.md): single-elimination 1v1 and 2v2
// tournaments on one installation's server, their entries, the bracket's matches, every round
// (one per attributed kill or admin decision), the champion's title and the prize payouts. The
// kill rows themselves are untouched: a round points at its kill (kill_id), and a kill is
// attributed to at most one round. Additive.
const TournamentSQL = `
CREATE TABLE IF NOT EXISTS tournaments (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT','SIGNUP','CHECKIN','LIVE','PAUSED','FINISHED','CANCELLED')),
    format TEXT NOT NULL DEFAULT 'SINGLE_ELIM' CHECK (format IN ('SINGLE_ELIM')),
    team_size SMALLINT NOT NULL DEFAULT 1 CHECK (team_size IN (1,2)),
    bracket_size SMALLINT NOT NULL DEFAULT 8 CHECK (bracket_size IN (4,8,16,32)),
    best_of SMALLINT NOT NULL DEFAULT 1 CHECK (best_of IN (1,3,5)),
    seeding TEXT NOT NULL DEFAULT 'RANDOM' CHECK (seeding IN ('RANDOM','RANKED')),
    starts_at TIMESTAMPTZ NOT NULL,
    signup_opens_at TIMESTAMPTZ,
    checkin_minutes INTEGER NOT NULL DEFAULT 30 CHECK (checkin_minutes >= 0 AND checkin_minutes <= 1440),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    rules JSONB NOT NULL DEFAULT '{}'::jsonb,
    prizes JSONB NOT NULL DEFAULT '[]'::jsonb,
    discord_channel_id TEXT,
    signup_message_id TEXT,
    bracket_message_id TEXT,
    created_by_discord_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_tournaments_server_status ON tournaments(server_id, status);
CREATE INDEX IF NOT EXISTS idx_tournaments_installation ON tournaments(installation_id, created_at DESC);

CREATE TABLE IF NOT EXISTS tournament_entries (
    id BIGSERIAL PRIMARY KEY,
    tournament_id BIGINT NOT NULL REFERENCES tournaments(id) ON DELETE CASCADE,
    seed INTEGER,
    team_no INTEGER NOT NULL,
    checked_in_at TIMESTAMPTZ,
    status TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','ELIMINATED','WINNER','DQ','WITHDRAWN')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tournament_id, team_no)
);

CREATE TABLE IF NOT EXISTS tournament_entry_players (
    entry_id BIGINT NOT NULL REFERENCES tournament_entries(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    discord_user_id TEXT NOT NULL,
    player_name TEXT NOT NULL,
    PRIMARY KEY (entry_id, player_id)
);
CREATE INDEX IF NOT EXISTS idx_tournament_entry_players_player ON tournament_entry_players(player_id);

CREATE TABLE IF NOT EXISTS tournament_matches (
    id BIGSERIAL PRIMARY KEY,
    tournament_id BIGINT NOT NULL REFERENCES tournaments(id) ON DELETE CASCADE,
    round INTEGER NOT NULL,
    position INTEGER NOT NULL,
    round_name TEXT NOT NULL,
    entry_a BIGINT REFERENCES tournament_entries(id) ON DELETE SET NULL,
    entry_b BIGINT REFERENCES tournament_entries(id) ON DELETE SET NULL,
    status TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','CALLED','LIVE','DONE','FORFEIT')),
    arena_no INTEGER,
    score_a INTEGER NOT NULL DEFAULT 0,
    score_b INTEGER NOT NULL DEFAULT 0,
    winner_entry BIGINT REFERENCES tournament_entries(id) ON DELETE SET NULL,
    next_match_id BIGINT REFERENCES tournament_matches(id) ON DELETE SET NULL,
    called_at TIMESTAMPTZ,
    started_at TIMESTAMPTZ,
    ended_at TIMESTAMPTZ,
    timer_ends_at TIMESTAMPTZ,
    UNIQUE (tournament_id, round, position)
);

CREATE TABLE IF NOT EXISTS tournament_rounds (
    id BIGSERIAL PRIMARY KEY,
    match_id BIGINT NOT NULL REFERENCES tournament_matches(id) ON DELETE CASCADE,
    n INTEGER NOT NULL,
    winner_entry BIGINT REFERENCES tournament_entries(id) ON DELETE SET NULL,
    kill_id BIGINT REFERENCES kills(id) ON DELETE SET NULL,
    killer_name TEXT NOT NULL DEFAULT '',
    victim_name TEXT NOT NULL DEFAULT '',
    weapon TEXT NOT NULL DEFAULT '',
    distance DOUBLE PRECISION,
    at TIMESTAMPTZ NOT NULL,
    counted BOOLEAN NOT NULL DEFAULT FALSE,
    flag TEXT CHECK (flag IS NULL OR flag IN ('WEAPON','OUTSIDE_ARENA','INTERFERENCE','NON_PLAYER','MANUAL')),
    decided_by TEXT
);
CREATE INDEX IF NOT EXISTS idx_tournament_rounds_match ON tournament_rounds(match_id, n);
CREATE UNIQUE INDEX IF NOT EXISTS uq_tournament_rounds_kill ON tournament_rounds(kill_id) WHERE kill_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS tournament_titles (
    installation_id BIGINT NOT NULL,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    title TEXT NOT NULL,
    tournament_id BIGINT NOT NULL REFERENCES tournaments(id) ON DELETE CASCADE,
    awarded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (installation_id, player_id)
);

CREATE TABLE IF NOT EXISTS tournament_payouts (
    tournament_id BIGINT NOT NULL REFERENCES tournaments(id) ON DELETE CASCADE,
    entry_id BIGINT NOT NULL REFERENCES tournament_entries(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    place INTEGER NOT NULL,
    points BIGINT NOT NULL,
    transaction_id BIGINT,
    paid_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tournament_id, entry_id, player_id)
);
`
