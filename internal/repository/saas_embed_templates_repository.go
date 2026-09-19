package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
)

// EmbedTemplateRepository persists installation_embed_templates. Every read and
// write is scoped by organization AND installation (the installation must belong to
// the organization), so an installation id alone can never reach another tenant's
// row. config_json is (un)marshalled here into the typed embedtemplates.Config;
// callers never see raw JSONB.
type EmbedTemplateRepository struct{ pool *pgxpool.Pool }

func NewEmbedTemplateRepository(pool *pgxpool.Pool) *EmbedTemplateRepository {
	return &EmbedTemplateRepository{pool: pool}
}

const embedTemplateCols = `t.id, t.installation_id, t.config_json, t.created_at, t.updated_at`

func scanEmbedTemplate(row pgx.Row) (embedtemplates.Stored, error) {
	var s embedtemplates.Stored
	var raw []byte
	if err := row.Scan(&s.ID, &s.InstallationID, &raw, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return embedtemplates.Stored{}, err
	}
	if err := json.Unmarshal(raw, &s.Config); err != nil {
		return embedtemplates.Stored{}, fmt.Errorf("decode stored embed template %d: %w", s.ID, err)
	}
	if s.Config.Fields == nil {
		s.Config.Fields = []embedtemplates.Field{}
	}
	return s, nil
}

// List returns the installation's custom templates ordered by route key.
func (r *EmbedTemplateRepository) List(ctx context.Context, organizationID, installationID int64) ([]embedtemplates.Stored, error) {
	rows, err := r.pool.Query(ctx, `
SELECT `+embedTemplateCols+`, t.route_key
FROM installation_embed_templates t
JOIN installations i ON i.id = t.installation_id
WHERE i.organization_id = $1 AND t.installation_id = $2
ORDER BY t.route_key`, organizationID, installationID)
	if err != nil {
		return nil, fmt.Errorf("list embed templates: %w", err)
	}
	defer rows.Close()
	out := []embedtemplates.Stored{}
	for rows.Next() {
		var s embedtemplates.Stored
		var raw []byte
		var routeKey string
		if err := rows.Scan(&s.ID, &s.InstallationID, &raw, &s.CreatedAt, &s.UpdatedAt, &routeKey); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &s.Config); err != nil {
			return nil, fmt.Errorf("decode stored embed template %d: %w", s.ID, err)
		}
		s.Config.RouteKey = routeKey // the column is authoritative for the key
		if s.Config.Fields == nil {
			s.Config.Fields = []embedtemplates.Field{}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Get returns nil, nil when the route has no custom template for this installation
// (including when the installation is not the organization's).
func (r *EmbedTemplateRepository) Get(ctx context.Context, organizationID, installationID int64, routeKey string) (*embedtemplates.Stored, error) {
	s, err := scanEmbedTemplate(r.pool.QueryRow(ctx, `
SELECT `+embedTemplateCols+`
FROM installation_embed_templates t
JOIN installations i ON i.id = t.installation_id
WHERE i.organization_id = $1 AND t.installation_id = $2 AND t.route_key = $3`, organizationID, installationID, routeKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get embed template: %w", err)
	}
	s.Config.RouteKey = routeKey
	return &s, nil
}

// Upsert inserts or replaces the template for (installation, route) in one atomic
// statement: the unique constraint makes concurrent saves converge on a single row,
// created_at keeps its original value and updated_at is refreshed on every write.
// The row is only written if the installation belongs to the organization;
// otherwise it returns embedtemplates.ErrInstallationNotFound.
func (r *EmbedTemplateRepository) Upsert(ctx context.Context, organizationID, installationID int64, cfg embedtemplates.Config) (embedtemplates.Stored, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return embedtemplates.Stored{}, fmt.Errorf("encode embed template: %w", err)
	}
	row := r.pool.QueryRow(ctx, `
WITH up AS (
    INSERT INTO installation_embed_templates (installation_id, route_key, config_json)
    SELECT i.id, $3, $4::jsonb FROM installations i WHERE i.id = $2 AND i.organization_id = $1
    ON CONFLICT (installation_id, route_key)
    DO UPDATE SET config_json = EXCLUDED.config_json, updated_at = NOW()
    RETURNING id, installation_id, config_json, created_at, updated_at
)
SELECT id, installation_id, config_json, created_at, updated_at FROM up`, organizationID, installationID, cfg.RouteKey, string(raw))
	s, err := scanEmbedTemplate(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return embedtemplates.Stored{}, embedtemplates.ErrInstallationNotFound
	}
	if err != nil {
		return embedtemplates.Stored{}, fmt.Errorf("upsert embed template: %w", err)
	}
	s.Config.RouteKey = cfg.RouteKey
	return s, nil
}

// Delete removes only this installation's row for the route. It reports whether a
// row existed; deleting nothing (or another tenant's row, which it cannot see) is
// not an error.
func (r *EmbedTemplateRepository) Delete(ctx context.Context, organizationID, installationID int64, routeKey string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
DELETE FROM installation_embed_templates t
USING installations i
WHERE i.id = t.installation_id AND i.organization_id = $1 AND t.installation_id = $2 AND t.route_key = $3`, organizationID, installationID, routeKey)
	if err != nil {
		return false, fmt.Errorf("delete embed template: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
