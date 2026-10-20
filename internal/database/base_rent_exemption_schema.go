package database

// BaseRentExemptionSQL lets the server owner make one player-requested base
// rent-free. A rent-free base pays no rent and is never paused; when rent is
// charged again its clock restarts from that moment. It also makes the due
// date the latest of the last paid day, when the base was registered, when
// rent was switched on and when rent resumed for it, so switching rent off
// and on again never leaves a base overdue straight away.
const BaseRentExemptionSQL = `
CREATE TABLE IF NOT EXISTS base_rent_exemptions (
 base_id BIGINT PRIMARY KEY REFERENCES case_registered_bases(id) ON DELETE CASCADE,
 installation_id BIGINT NOT NULL,
 exempt BOOLEAN NOT NULL,
 note TEXT NOT NULL DEFAULT '' CHECK (char_length(note) <= 200),
 changed_by_user_id BIGINT,
 changed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE OR REPLACE FUNCTION base_rent_due_at(p_base BIGINT) RETURNS TIMESTAMPTZ
LANGUAGE sql STABLE AS $$
 SELECT GREATEST(COALESCE((SELECT MAX(rp.ends_at) FROM base_rent_payments rp WHERE rp.base_id=b.id),'-infinity'::TIMESTAMPTZ),
                 b.created_at, rs.enabled_since, COALESCE(ex.changed_at,'-infinity'::TIMESTAMPTZ))
 FROM case_registered_bases b
 JOIN base_rent_settings rs ON rs.installation_id=b.installation_id AND rs.server_id=b.server_id AND rs.enabled
 LEFT JOIN base_rent_exemptions ex ON ex.base_id=b.id
 WHERE b.id=p_base AND b.state<>'REVOKED' AND NOT COALESCE(ex.exempt,FALSE)
  AND EXISTS (SELECT 1 FROM case_base_requests rq WHERE rq.base_id=b.id AND rq.status='APPROVED')
$$;
`
