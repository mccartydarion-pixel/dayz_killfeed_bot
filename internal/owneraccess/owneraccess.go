// Package owneraccess answers one question from memory: does this organization (or this
// installation) belong to a platform owner?
//
// A platform owner is a Discord account on CHAMPION_ADMIN_DISCORD_IDS. An organization belongs
// to one when its owner (organizations.owner_user_id) is such an account. Being an ADMIN or
// MEMBER of somebody else's organization does not count. For those organizations every plan
// feature is unlocked (internal/entitlements) and feature switches default to on
// (internal/featureflags); see docs/ADMIN_API.md "Platform owner access".
//
// The answer is a snapshot of every such organization and installation, reloaded in the
// background on a short TTL, so hot paths (a feed route per event, an embed render per card)
// never add a query. A change of ownership or a newly created organization is therefore seen
// within one TTL, or at once when the caller invokes Refresh.
package owneraccess

import (
	"context"
	"sync"
	"time"
)

// Store loads the snapshot; *repository.PlatformOwnerRepository implements it.
type Store interface {
	PlatformOwnerScope(ctx context.Context, ownerDiscordIDs []string) (organizationIDs, installationIDs []int64, err error)
}

// DefaultTTL is how long a snapshot is trusted before a background reload.
const DefaultTTL = 60 * time.Second

// Resolver holds the snapshot. A nil *Resolver answers false to everything.
type Resolver struct {
	store  Store
	owners []string
	ttl    time.Duration
	now    func() time.Time

	loadMu        sync.Mutex // serializes loads, so an older load never lands after a newer one
	mu            sync.RWMutex
	organizations map[int64]bool
	installations map[int64]bool
	loadedAt      time.Time
	refreshing    bool
}

// New builds a resolver for the given platform-owner Discord ids. With no ids (or no store)
// nobody is a platform owner and the store is never queried.
func New(store Store, ownerDiscordIDs []string, ttl time.Duration) *Resolver {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	owners := append([]string(nil), ownerDiscordIDs...)
	return &Resolver{store: store, owners: owners, ttl: ttl, now: time.Now, organizations: map[int64]bool{}, installations: map[int64]bool{}}
}

func (r *Resolver) active() bool { return r != nil && r.store != nil && len(r.owners) > 0 }

// Refresh reloads the snapshot now. On an error the previous snapshot is kept.
func (r *Resolver) Refresh(ctx context.Context) error {
	if !r.active() {
		return nil
	}
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	orgs, insts, err := r.store.PlatformOwnerScope(ctx, r.owners)
	if err != nil {
		return err
	}
	organizations := make(map[int64]bool, len(orgs))
	for _, id := range orgs {
		organizations[id] = true
	}
	installations := make(map[int64]bool, len(insts))
	for _, id := range insts {
		installations[id] = true
	}
	r.mu.Lock()
	r.organizations, r.installations, r.loadedAt, r.refreshing = organizations, installations, r.now(), false
	r.mu.Unlock()
	return nil
}

// ensureFresh starts a background reload when the snapshot is stale; it never blocks.
func (r *Resolver) ensureFresh() {
	r.mu.Lock()
	stale := r.now().Sub(r.loadedAt) > r.ttl && !r.refreshing
	if stale {
		r.refreshing = true
	}
	r.mu.Unlock()
	if !stale {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.Refresh(ctx); err != nil {
			r.mu.Lock()
			r.refreshing = false
			r.mu.Unlock()
		}
	}()
}

// Organization reports whether organizationID is owned by a platform owner.
func (r *Resolver) Organization(organizationID int64) bool {
	if !r.active() || organizationID <= 0 {
		return false
	}
	r.ensureFresh()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.organizations[organizationID]
}

// Installation reports whether installationID belongs to an organization owned by a platform
// owner.
func (r *Resolver) Installation(installationID int64) bool {
	if !r.active() || installationID <= 0 {
		return false
	}
	r.ensureFresh()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.installations[installationID]
}
