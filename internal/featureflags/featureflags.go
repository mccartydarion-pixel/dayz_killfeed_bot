// Package featureflags resolves per-installation feature overrides set by the platform owner
// (Owner Hub "Feature flags", docs/ADMIN_API.md). Each flag has an environment default - the
// switch that existed before - and an optional per-installation override stored in
// installation_feature_flags. An override wins over the default; no override means the
// default applies, exactly as before this package existed.
//
// One rule sits between the two: for an installation of a platform owner's own organization
// the default of a flag marked OwnerDefaultOn is on. An override still wins, so the owner can
// switch a feature off for their own server.
//
// The resolver keeps every override in memory and refreshes it in the background on a short
// TTL, so hot paths (an embed render per event, a worker start) never wait on the database.
package featureflags

import (
	"context"
	"sync"
	"time"
)

// Flag keys. The key is what the API and the table store; never rename one.
const (
	CustomEmbeds      = "custom_embeds"
	ShopCanary        = "shop_canary"
	CaseEvidence      = "case_evidence"
	CaseBuildEvidence = "case_build_evidence"
	// MapRotation lets an installation use map rotation with a player vote (docs/MAP_ROTATION.md).
	// Default off: without it nothing of the feature runs and no server file is ever written.
	MapRotation = "map_rotation"
)

// Definition describes one flag for the Owner Hub.
type Definition struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// EnvVar names the environment default the override replaces.
	EnvVar string `json:"envVar"`
	// RestartRequired: the flag is read when a worker starts, so a change takes effect on
	// the installation's next worker restart (the Owner Hub offers that button).
	RestartRequired bool `json:"restartRequired"`
	// OwnerDefaultOn: for an installation of a platform owner's own organization the flag
	// is on unless an override says otherwise (docs/ADMIN_API.md "Platform owner access").
	// Only set it for a flag where "on" simply unlocks a feature the owner then configures.
	// A flag that makes the bot start doing something by itself (real shop deliveries,
	// evidence collection with high write volume) or that needs a worker restart stays
	// false and says why in OwnerDefaultNote.
	OwnerDefaultOn bool `json:"ownerDefaultOn"`
	// OwnerDefaultNote explains, for the Owner Hub, why a flag is not switched on by owner
	// access. Empty when OwnerDefaultOn is true.
	OwnerDefaultNote string `json:"ownerDefaultNote,omitempty"`
}

// Catalog is every flag the owner can override, in display order.
var Catalog = []Definition{
	{Key: CustomEmbeds, Label: "Custom embeds", Description: "Let this installation's saved embed templates render live feed cards instead of the Champion defaults.", EnvVar: "CHAMPION_CUSTOM_EMBEDS_ENABLED", OwnerDefaultOn: true},
	{Key: ShopCanary, Label: "Shop canary execution", Description: "Allow real shop fulfilment operations (Pay-to-win canary) for this installation.", EnvVar: "CHAMPION_SHOP_CANARY_EXECUTION / _INSTALLATION_IDS",
		OwnerDefaultNote: "Not switched on by owner access: it lets real shop deliveries change files on the game server, and the canary needs its own preparation. Set an override to use it."},
	{Key: CaseEvidence, Label: "C.A.S.E. evidence", Description: "Collect anti-cheat evidence (sessions, movement, detectors) for this installation's server. High write volume.", EnvVar: "CASE_EVIDENCE_ENABLED / _SERVER_IDS", RestartRequired: true,
		OwnerDefaultNote: "Not switched on by owner access: it starts collecting evidence by itself with high write volume, and only takes effect after a worker restart. Set an override to use it."},
	{Key: CaseBuildEvidence, Label: "C.A.S.E. build evidence", Description: "Also collect build-action evidence. Requires C.A.S.E. evidence.", EnvVar: "CASE_BUILD_EVIDENCE_ENABLED / _SERVER_IDS", RestartRequired: true,
		OwnerDefaultNote: "Not switched on by owner access: it depends on C.A.S.E. evidence, adds more writes, and only takes effect after a worker restart. Set an override to use it."},
	{Key: MapRotation, Label: "Map rotation", Description: "Let this installation's owner set up map rotation with a player vote. When it runs, Champion writes cfggameplay.json and cfgplayerspawnpoints.xml on the game server.", EnvVar: "CHAMPION_MAP_ROTATION_ENABLED", OwnerDefaultOn: true},
}

// OwnerDefaultOn reports whether key is switched on by platform-owner access.
func OwnerDefaultOn(key string) bool {
	for _, d := range Catalog {
		if d.Key == key {
			return d.OwnerDefaultOn
		}
	}
	return false
}

// Known reports whether key is a catalog flag.
func Known(key string) bool {
	for _, d := range Catalog {
		if d.Key == key {
			return true
		}
	}
	return false
}

// Override is one stored per-installation decision.
type Override struct {
	InstallationID int64
	Flag           string
	Enabled        bool
	Reason         string
	UpdatedBy      string
	UpdatedAt      time.Time
}

// Store is what the resolver reads: every override, and the game server each installation
// runs (so server-scoped flags resolve through the installation that owns the server).
type Store interface {
	ListOverrides(ctx context.Context) ([]Override, error)
	ServerInstallations(ctx context.Context) (map[int64]int64, error)
}

// DefaultTTL is how long a loaded snapshot is trusted before a background refresh.
const DefaultTTL = 30 * time.Second

// Resolver answers flag questions from an in-memory snapshot.
type Resolver struct {
	store Store
	ttl   time.Duration
	now   func() time.Time

	// loadMu serializes loads so a background refresh that started earlier can never land
	// after (and overwrite) a forced Refresh that followed a write.
	loadMu     sync.Mutex
	mu         sync.RWMutex
	overrides  map[int64]map[string]bool // installationID -> flag -> enabled
	servers    map[int64]int64           // gameServerID -> installationID
	loadedAt   time.Time
	refreshing bool

	// ownerInstallation answers, from memory, whether an installation belongs to a platform
	// owner's own organization (internal/owneraccess). nil means none does.
	ownerInstallation func(installationID int64) bool
}

// SetOwnerInstallations installs the platform-owner lookup. Call it once, before the resolver
// is shared between goroutines.
func (r *Resolver) SetOwnerInstallations(fn func(installationID int64) bool) {
	if r != nil {
		r.ownerInstallation = fn
	}
}

// OwnerAccess reports whether flag is on for installationID because the installation belongs
// to a platform owner's own organization. It ignores overrides: Enabled applies those first.
func (r *Resolver) OwnerAccess(installationID int64, flag string) bool {
	if r == nil || installationID <= 0 || r.ownerInstallation == nil || !OwnerDefaultOn(flag) {
		return false
	}
	return r.ownerInstallation(installationID)
}

// New builds a resolver; it loads nothing until Refresh or the first question.
func New(store Store, ttl time.Duration) *Resolver {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Resolver{store: store, ttl: ttl, now: time.Now, overrides: map[int64]map[string]bool{}, servers: map[int64]int64{}}
}

// Refresh reloads the snapshot now (the owner API calls it after a write).
func (r *Resolver) Refresh(ctx context.Context) error {
	if r == nil || r.store == nil {
		return nil
	}
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	rows, err := r.store.ListOverrides(ctx)
	if err != nil {
		return err
	}
	servers, err := r.store.ServerInstallations(ctx)
	if err != nil {
		return err
	}
	overrides := make(map[int64]map[string]bool, len(rows))
	for _, o := range rows {
		m := overrides[o.InstallationID]
		if m == nil {
			m = map[string]bool{}
			overrides[o.InstallationID] = m
		}
		m[o.Flag] = o.Enabled
	}
	r.mu.Lock()
	r.overrides, r.servers, r.loadedAt, r.refreshing = overrides, servers, r.now(), false
	r.mu.Unlock()
	return nil
}

// ensureFresh kicks off a background refresh when the snapshot is stale; it never blocks.
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

// Enabled answers for one installation: the override when there is one; else on when the
// installation has platform-owner access to the flag (OwnerAccess); else fallback.
func (r *Resolver) Enabled(installationID int64, flag string, fallback bool) bool {
	if r == nil || installationID <= 0 {
		return fallback
	}
	r.ensureFresh()
	r.mu.RLock()
	if m := r.overrides[installationID]; m != nil {
		if v, ok := m[flag]; ok {
			r.mu.RUnlock()
			return v
		}
	}
	r.mu.RUnlock()
	return fallback || r.OwnerAccess(installationID, flag)
}

// EnabledForServer answers for the installation that owns gameServerID.
func (r *Resolver) EnabledForServer(gameServerID int64, flag string, fallback bool) bool {
	if r == nil || gameServerID <= 0 {
		return fallback
	}
	r.ensureFresh()
	r.mu.RLock()
	inst := r.servers[gameServerID]
	r.mu.RUnlock()
	if inst == 0 {
		return fallback
	}
	return r.Enabled(inst, flag, fallback)
}

// Overrides returns the stored decisions for one installation (flag -> enabled).
func (r *Resolver) Overrides(installationID int64) map[string]bool {
	out := map[string]bool{}
	if r == nil {
		return out
	}
	r.ensureFresh()
	r.mu.RLock()
	defer r.mu.RUnlock()
	for k, v := range r.overrides[installationID] {
		out[k] = v
	}
	return out
}
