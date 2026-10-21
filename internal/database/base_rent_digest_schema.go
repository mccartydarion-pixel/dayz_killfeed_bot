package database

// BaseRentDigestSQL records when each server last got the daily staff notice
// listing bases that were paused for unpaid rent, so it's sent at most once
// a day. Additive (one table).
const BaseRentDigestSQL = `
CREATE TABLE IF NOT EXISTS base_rent_digests (
 installation_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 last_sent_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY (installation_id,server_id)
);
`
