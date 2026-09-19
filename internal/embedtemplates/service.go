package embedtemplates

import (
	"context"
	"errors"
)

var (
	// ErrInvalidRoute: the route key is not one templates can be stored for.
	ErrInvalidRoute = errors.New("unknown embed template route")
	// ErrInstallationNotFound: the installation does not belong to the organization
	// (deliberately the same answer as "does not exist").
	ErrInstallationNotFound = errors.New("installation not found")
)

// Store is the persistence the Service needs (implemented by
// *repository.EmbedTemplateRepository). Every method is scoped by BOTH the
// organization and the installation: an installation id alone never selects a row.
type Store interface {
	List(ctx context.Context, organizationID, installationID int64) ([]Stored, error)
	Get(ctx context.Context, organizationID, installationID int64, routeKey string) (*Stored, error)
	Upsert(ctx context.Context, organizationID, installationID int64, cfg Config) (Stored, error)
	Delete(ctx context.Context, organizationID, installationID int64, routeKey string) (bool, error)
}

// Service validates and normalizes; the HTTP handlers stay thin. Authorization
// (who may call) is decided by the handlers before they reach it.
type Service struct{ store Store }

func NewService(store Store) *Service { return &Service{store: store} }

// List returns the installation's customized templates (routes without a custom
// template are simply absent - defaults are never copied into the database).
func (s *Service) List(ctx context.Context, organizationID, installationID int64) ([]Stored, error) {
	return s.store.List(ctx, organizationID, installationID)
}

// Get returns nil, nil when the route has no custom template.
func (s *Service) Get(ctx context.Context, organizationID, installationID int64, routeKey string) (*Stored, error) {
	if !ValidRoute(routeKey) {
		return nil, ErrInvalidRoute
	}
	return s.store.Get(ctx, organizationID, installationID, routeKey)
}

// Save validates cfg for routeKey and upserts the normalized result.
func (s *Service) Save(ctx context.Context, organizationID, installationID int64, routeKey string, cfg Config) (Stored, error) {
	if !ValidRoute(routeKey) {
		return Stored{}, ErrInvalidRoute
	}
	normalized, err := Validate(cfg, routeKey)
	if err != nil {
		return Stored{}, err
	}
	return s.store.Upsert(ctx, organizationID, installationID, normalized)
}

// Delete resets the route to the Champion default. Idempotent: it reports whether a
// custom template existed, and deleting nothing is not an error.
func (s *Service) Delete(ctx context.Context, organizationID, installationID int64, routeKey string) (bool, error) {
	if !ValidRoute(routeKey) {
		return false, ErrInvalidRoute
	}
	return s.store.Delete(ctx, organizationID, installationID, routeKey)
}
