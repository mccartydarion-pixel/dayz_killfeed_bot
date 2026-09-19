package factionhub

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// The approved visual catalogs. The BACKEND is authoritative: the website mirrors these keys
// for its pickers, but every write is validated here, so a client that skips the picker (or a
// stale catalog) can never store an arbitrary string. Keys are stable identifiers, upper-case
// and URL/CSS-free; values are never URLs. Add a key here (and to docs/FACTIONS.md) before the
// website may offer it.

// DayzFlags are the approved DayZ flag keys.
var DayzFlags = []string{"BLACK", "BLUE", "GREEN", "RED"}

// Armbands are the approved armband color keys.
var Armbands = []string{"BLACK", "BLUE", "GREEN", "ORANGE", "PINK", "RED", "WHITE", "YELLOW"}

func inCatalog(catalog []string, key string) bool {
	i := sort.SearchStrings(catalog, key)
	return i < len(catalog) && catalog[i] == key
}

// ValidateFlagKey normalizes (trim, upper-case) and validates a flag key against DayzFlags.
// "" is valid and means "no flag" (it clears the stored key).
func ValidateFlagKey(raw string) (string, error) {
	k := strings.ToUpper(strings.TrimSpace(raw))
	if k == "" {
		return "", nil
	}
	if !inCatalog(DayzFlags, k) {
		return "", &ValidationError{Issues: []string{"flagKey must be one of the approved DayZ flags: " + strings.Join(DayzFlags, ", ")}}
	}
	return k, nil
}

// ValidateArmbandKey normalizes and validates an armband key against Armbands. "" clears it.
func ValidateArmbandKey(raw string) (string, error) {
	k := strings.ToUpper(strings.TrimSpace(raw))
	if k == "" {
		return "", nil
	}
	if !inCatalog(Armbands, k) {
		return "", &ValidationError{Issues: []string{"armbandKey must be one of the approved armbands: " + strings.Join(Armbands, ", ")}}
	}
	return k, nil
}

// Asset is a stored faction asset's metadata (today only LOGO). The bytes live behind
// assetstore.Store under StorageKey; PublicID is the unguessable id in the public URL.
type Asset struct {
	ID               int64
	PublicID         string
	FactionID        int64
	StorageKey       string
	ContentType      string
	SizeBytes        int
	Width, Height    int
	OriginalFilename string
	CreatedAt        time.Time
}

// AssetTypeLogo is the only asset type in Phase 4.
const AssetTypeLogo = "LOGO"

// Phase 4 errors.
var (
	// ErrLeadershipTransferRequired: the LEADER cannot leave while still leader.
	ErrLeadershipTransferRequired = errors.New("the leader must transfer leadership before leaving")
	// ErrAlreadyLeader: the transfer target already leads the faction.
	ErrAlreadyLeader = errors.New("the member is already the leader")
)
