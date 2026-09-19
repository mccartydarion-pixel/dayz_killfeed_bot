package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/assetstore"
	"github.com/yourname/dayz-killfeed/internal/factionhub"
)

// Faction Hub Phase 4 (docs/FACTIONS.md): logo metadata, leadership transfer and self-leave.
// Same rules as the rest of the Hub repository: every method is scoped by organization AND
// installation AND faction (an asset is never authorized by its own id), authorization is read
// inside the mutating transaction, and errors are the typed factionhub errors.

// NewLogoAsset is the metadata of an already-stored, already-validated logo image.
type NewLogoAsset struct {
	PublicID         string // UUID, also embedded in StorageKey
	StorageKey       string // server-generated; never derived from an uploaded name
	ContentType      string
	OriginalFilename string // sanitized, display/debug only
	SizeBytes        int
	Width, Height    int
}

const hubAssetCols = `a.id, a.public_id::text, a.faction_id, a.storage_key, a.content_type, a.size_bytes, a.width, a.height, a.original_filename, a.created_at`

func scanHubAsset(row interface{ Scan(...any) error }) (factionhub.Asset, error) {
	var a factionhub.Asset
	err := row.Scan(&a.ID, &a.PublicID, &a.FactionID, &a.StorageKey, &a.ContentType, &a.SizeBytes, &a.Width, &a.Height, &a.OriginalFilename, &a.CreatedAt)
	return a, err
}

// RequireLeader checks, without changing anything, that the faction exists within the
// organization + installation and that userID is its LEADER. The upload handler calls it
// BEFORE reading a multi-megabyte body, so an unauthorized caller costs one query, not an
// upload; ReplaceLogo re-checks inside its transaction.
func (r *FactionHubRepository) RequireLeader(ctx context.Context, organizationID, installationID, factionID, userID int64) error {
	if _, err := hubFactionScoped(ctx, r.pool, organizationID, installationID, factionID, ""); err != nil {
		return err
	}
	role, err := hubRole(ctx, r.pool, factionID, userID)
	if err != nil {
		return err
	}
	if !factionhub.CanEditFaction(role) {
		return factionhub.ErrForbidden
	}
	return nil
}

// ReplaceLogo records a new logo for the faction and returns the previous asset (nil if there
// was none) so the caller can delete its bytes AFTER this commit. In one transaction: lock
// the faction, require the LEADER, insert the asset row, repoint the faction, delete the old
// row. The old bytes are never touched here.
func (r *FactionHubRepository) ReplaceLogo(ctx context.Context, organizationID, installationID, factionID, actorUserID int64, in NewLogoAsset) (factionhub.Asset, *factionhub.Asset, error) {
	var created factionhub.Asset
	var old *factionhub.Asset
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		f, err := hubFactionScoped(ctx, tx, organizationID, installationID, factionID, "FOR UPDATE OF f")
		if err != nil {
			return err
		}
		role, err := hubRole(ctx, tx, factionID, actorUserID)
		if err != nil {
			return err
		}
		if !factionhub.CanEditFaction(role) {
			return factionhub.ErrForbidden
		}
		old = f.Logo
		var id int64
		err = tx.QueryRow(ctx, `
INSERT INTO hub_faction_assets(public_id, organization_id, installation_id, faction_id, asset_type, storage_key, content_type, size_bytes, width, height, original_filename, created_by_user_id)
VALUES($1::uuid,$2,$3,$4,'LOGO',$5,$6,$7,$8,$9,$10,$11)
RETURNING id`, in.PublicID, organizationID, installationID, factionID, in.StorageKey, in.ContentType, in.SizeBytes, in.Width, in.Height, in.OriginalFilename, actorUserID).Scan(&id)
		if err != nil {
			return fmt.Errorf("hub insert logo asset: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE hub_factions SET logo_asset_id=$1, updated_at=NOW() WHERE id=$2`, id, factionID); err != nil {
			return fmt.Errorf("hub point logo: %w", err)
		}
		if old != nil {
			if _, err := tx.Exec(ctx, `DELETE FROM hub_faction_assets WHERE id=$1 AND faction_id=$2`, old.ID, factionID); err != nil {
				return fmt.Errorf("hub drop old logo asset: %w", err)
			}
		}
		created, err = scanHubAsset(tx.QueryRow(ctx, `SELECT `+hubAssetCols+` FROM hub_faction_assets a WHERE a.id=$1 AND a.faction_id=$2`, id, factionID))
		return err
	})
	if err != nil {
		return factionhub.Asset{}, nil, err
	}
	return created, old, nil
}

// DeleteLogo removes the faction's logo (LEADER only) and returns the removed asset, or nil
// when there was none (idempotent: no logo is not an error). The bytes are deleted by the caller
// after the commit.
func (r *FactionHubRepository) DeleteLogo(ctx context.Context, organizationID, installationID, factionID, actorUserID int64) (*factionhub.Asset, error) {
	var old *factionhub.Asset
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		f, err := hubFactionScoped(ctx, tx, organizationID, installationID, factionID, "FOR UPDATE OF f")
		if err != nil {
			return err
		}
		role, err := hubRole(ctx, tx, factionID, actorUserID)
		if err != nil {
			return err
		}
		if !factionhub.CanEditFaction(role) {
			return factionhub.ErrForbidden
		}
		if f.Logo == nil {
			return nil
		}
		old = f.Logo
		if _, err := tx.Exec(ctx, `UPDATE hub_factions SET logo_asset_id=NULL, updated_at=NOW() WHERE id=$1`, factionID); err != nil {
			return fmt.Errorf("hub clear logo: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM hub_faction_assets WHERE id=$1 AND faction_id=$2`, old.ID, factionID); err != nil {
			return fmt.Errorf("hub drop logo asset: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return old, nil
}

// AssetByPublicID resolves a public logo id (the unguessable UUID in the public URL) to its
// metadata, or ErrNotFound. The public endpoint is the only caller: logos are public profile
// assets, and a replaced or deleted logo has no row, so its URL stops resolving.
func (r *FactionHubRepository) AssetByPublicID(ctx context.Context, publicID string) (*factionhub.Asset, error) {
	a, err := scanHubAsset(r.pool.QueryRow(ctx, `SELECT `+hubAssetCols+` FROM hub_faction_assets a WHERE a.public_id=$1::uuid AND a.asset_type='LOGO'`, publicID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, factionhub.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("hub asset lookup: %w", err)
	}
	return &a, nil
}

// ReferencedAssetKeys returns which of keys are referenced by an asset row (orphan cleanup).
func (r *FactionHubRepository) ReferencedAssetKeys(ctx context.Context, keys []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT storage_key FROM hub_faction_assets WHERE storage_key = ANY($1)`, keys)
	if err != nil {
		return nil, fmt.Errorf("hub referenced asset keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// TransferLeadership makes the target member the LEADER and the current leader an OFFICER,
// atomically. The faction row is locked, so two simultaneous transfers serialize; the loser
// finds the actor is no longer LEADER (ErrForbidden). The one-leader partial unique index is
// the backstop: the old leader is demoted before the target is promoted, and both happen in
// one transaction, so the faction never has zero or two leaders. Only the current LEADER may
// transfer, to a MEMBER or OFFICER of the SAME faction.
func (r *FactionHubRepository) TransferLeadership(ctx context.Context, organizationID, installationID, factionID, actorUserID, targetMemberID int64) (newLeader, previousLeader HubMember, err error) {
	err = r.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := hubFactionScoped(ctx, tx, organizationID, installationID, factionID, "FOR UPDATE OF f"); err != nil {
			return err
		}
		var actorMemberID int64
		var actorRole string
		if err := tx.QueryRow(ctx, `SELECT id, role_key FROM hub_faction_members WHERE faction_id=$1 AND user_id=$2 FOR UPDATE`, factionID, actorUserID).Scan(&actorMemberID, &actorRole); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return factionhub.ErrForbidden
			}
			return fmt.Errorf("hub transfer actor: %w", err)
		}
		if actorRole != factionhub.RoleLeader {
			return factionhub.ErrForbidden
		}
		target, err := hubMemberByID(ctx, tx, factionID, targetMemberID, "FOR UPDATE OF m")
		if err != nil {
			return err // not a member of THIS faction: not found
		}
		if target.ID == actorMemberID || target.RoleKey == factionhub.RoleLeader {
			return factionhub.ErrAlreadyLeader
		}
		if _, err := tx.Exec(ctx, `UPDATE hub_faction_members SET role_key='OFFICER' WHERE id=$1 AND faction_id=$2`, actorMemberID, factionID); err != nil {
			return fmt.Errorf("hub transfer demote: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE hub_faction_members SET role_key='LEADER' WHERE id=$1 AND faction_id=$2`, target.ID, factionID); err != nil {
			return fmt.Errorf("hub transfer promote: %w", err)
		}
		if newLeader, err = hubMemberByID(ctx, tx, factionID, target.ID, ""); err != nil {
			return err
		}
		previousLeader, err = hubMemberByID(ctx, tx, factionID, actorMemberID, "")
		return err
	})
	if err != nil {
		return HubMember{}, HubMember{}, err
	}
	return newLeader, previousLeader, nil
}

// LeaveFaction removes the acting user's own membership. A MEMBER or OFFICER may leave; the
// LEADER may not (ErrLeadershipTransferRequired) and must transfer leadership first. A
// non-member gets ErrForbidden, so a repeated leave is a safe 403 rather than a silent no-op.
// Application history is untouched; the user is immediately eligible to join or found a faction
// on the installation again.
func (r *FactionHubRepository) LeaveFaction(ctx context.Context, organizationID, installationID, factionID, userID int64) (*HubMember, error) {
	var left HubMember
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		// The user lock comes first (see lockHubUser), then the faction row, then the member row.
		if err := lockHubUser(ctx, tx, installationID, userID); err != nil {
			return err
		}
		if _, err := hubFactionScoped(ctx, tx, organizationID, installationID, factionID, "FOR UPDATE OF f"); err != nil {
			return err
		}
		var memberID int64
		var role string
		err := tx.QueryRow(ctx, `SELECT id, role_key FROM hub_faction_members WHERE faction_id=$1 AND user_id=$2 FOR UPDATE`, factionID, userID).Scan(&memberID, &role)
		if errors.Is(err, pgx.ErrNoRows) {
			return factionhub.ErrForbidden
		}
		if err != nil {
			return fmt.Errorf("hub leave lookup: %w", err)
		}
		if role == factionhub.RoleLeader {
			return factionhub.ErrLeadershipTransferRequired
		}
		if left, err = hubMemberByID(ctx, tx, factionID, memberID, ""); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM hub_faction_members WHERE id=$1`, memberID); err != nil {
			return fmt.Errorf("hub leave: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &left, nil
}

// --- PostgreSQL-backed assetstore.Store ---------------------------------------------------------

// PostgresAssetStore is Champion's durable assetstore.Store: objects are rows of
// hub_asset_blobs in the same PostgreSQL database (which has a persistent volume; the service
// filesystem does not). It has no public URL of its own, so Champion serves the objects
// through the public logo endpoint.
type PostgresAssetStore struct{ pool *pgxpool.Pool }

func NewPostgresAssetStore(pool *pgxpool.Pool) *PostgresAssetStore {
	return &PostgresAssetStore{pool: pool}
}

var _ assetstore.Store = (*PostgresAssetStore)(nil)

func (s *PostgresAssetStore) Put(ctx context.Context, key, contentType string, data []byte) error {
	if !assetstore.ValidKey(key) {
		return assetstore.ErrInvalidKey
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO hub_asset_blobs(storage_key, content_type, data) VALUES($1,$2,$3)
ON CONFLICT (storage_key) DO UPDATE SET content_type=EXCLUDED.content_type, data=EXCLUDED.data, created_at=NOW()`, key, contentType, data)
	if err != nil {
		return fmt.Errorf("asset put: %w", err)
	}
	return nil
}

func (s *PostgresAssetStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	if !assetstore.ValidKey(key) {
		return nil, "", assetstore.ErrInvalidKey
	}
	var data []byte
	var ct string
	err := s.pool.QueryRow(ctx, `SELECT data, content_type FROM hub_asset_blobs WHERE storage_key=$1`, key).Scan(&data, &ct)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", assetstore.ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("asset get: %w", err)
	}
	return data, ct, nil
}

func (s *PostgresAssetStore) Delete(ctx context.Context, key string) error {
	if !assetstore.ValidKey(key) {
		return assetstore.ErrInvalidKey
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM hub_asset_blobs WHERE storage_key=$1`, key); err != nil {
		return fmt.Errorf("asset delete: %w", err)
	}
	return nil
}

func (s *PostgresAssetStore) List(ctx context.Context, prefix string, olderThan time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	pattern := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix) + "%"
	rows, err := s.pool.Query(ctx, `SELECT storage_key FROM hub_asset_blobs WHERE storage_key LIKE $1 ESCAPE '\' AND created_at < $2 ORDER BY created_at, storage_key LIMIT $3`, pattern, olderThan, limit)
	if err != nil {
		return nil, fmt.Errorf("asset list: %w", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (s *PostgresAssetStore) URL(string) (string, bool) { return "", false }
