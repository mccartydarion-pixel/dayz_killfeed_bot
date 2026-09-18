package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CredentialAlgorithm is the fixed encryption algorithm for every envelope
// stored via CredentialRepository. Not a column: only one algorithm is
// supported today, so it's a constant rather than a per-row value that would
// need backfilling if it ever changed.
const CredentialAlgorithm = "AES-256-GCM"

// CredentialEnvelope is an encrypted Nitrado credential, ready to persist.
// Only the encrypted envelope ever passes through this repository - it never
// accepts or returns a plaintext token (section 16: repository APIs must
// never expose decrypted secrets).
//
// This reuses nitrado_connections (see NitradoConnection in
// server_repository.go) instead of a separate nitrado_credentials table:
// "iv" is Nonce (credential_nonce), the AEAD auth tag is embedded in
// Ciphertext (standard Go crypto/cipher AEAD.Seal output - Postgres GCM
// implementations append the tag to the ciphertext rather than needing a
// separate column), and "algorithm" is CredentialAlgorithm above rather than
// a stored column.
type CredentialEnvelope struct {
	ID             int64
	OrganizationID int64
	Ciphertext     []byte
	Nonce          []byte
	KeyVersion     int
	Status         string
}

type CredentialRepository struct{ pool *pgxpool.Pool }

func NewCredentialRepository(pool *pgxpool.Pool) *CredentialRepository {
	return &CredentialRepository{pool: pool}
}

// InsertForOrganization stores an encrypted credential envelope on
// guildID's nitrado_connections row (guild-scoped, matching the existing
// per-guild credential model - see ServerRepository.SaveConnection) and
// records organizationID as its SaaS owner.
func (r *CredentialRepository) InsertForOrganization(ctx context.Context, guildID int64, e CredentialEnvelope) error {
	const q = `
INSERT INTO nitrado_connections(guild_id, credential_ciphertext, credential_nonce, credential_key_version, status, organization_id)
VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT(guild_id) DO UPDATE SET
    credential_ciphertext=EXCLUDED.credential_ciphertext,
    credential_nonce=EXCLUDED.credential_nonce,
    credential_key_version=EXCLUDED.credential_key_version,
    status=EXCLUDED.status,
    organization_id=EXCLUDED.organization_id,
    updated_at=NOW()`
	if _, err := r.pool.Exec(ctx, q, guildID, e.Ciphertext, e.Nonce, e.KeyVersion, e.Status, e.OrganizationID); err != nil {
		return fmt.Errorf("insert credential envelope: %w", err)
	}
	return nil
}

// GetScoped returns the encrypted envelope for guildID, requiring it belong
// to organizationID (section 15 tenant isolation). Still only ever the
// encrypted envelope - callers decrypt, if authorized, outside this
// repository.
func (r *CredentialRepository) GetScoped(ctx context.Context, organizationID, guildID int64) (*CredentialEnvelope, error) {
	const q = `SELECT id, COALESCE(organization_id,0), credential_ciphertext, credential_nonce, credential_key_version, status FROM nitrado_connections WHERE guild_id=$1 AND organization_id=$2`
	var e CredentialEnvelope
	err := r.pool.QueryRow(ctx, q, guildID, organizationID).Scan(&e.ID, &e.OrganizationID, &e.Ciphertext, &e.Nonce, &e.KeyVersion, &e.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get scoped credential: %w", err)
	}
	return &e, nil
}

// DeleteForOrganization removes an organization's claimed credential
// envelope (rotation/disconnect), scoped so it can only ever affect a row
// this organization owns.
func (r *CredentialRepository) DeleteForOrganization(ctx context.Context, organizationID, guildID int64) error {
	if _, err := r.pool.Exec(ctx, `DELETE FROM nitrado_connections WHERE guild_id=$1 AND organization_id=$2`, guildID, organizationID); err != nil {
		return fmt.Errorf("delete credential envelope: %w", err)
	}
	return nil
}
