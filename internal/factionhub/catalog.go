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

// DayzFlags are the approved DayZ flag keys: the 33 flags the game ships (Chernarus, Livonia
// and the ones both maps share), keyed by the in-game class name without its "Flag_" prefix,
// upper-cased (Flag_BabyDeer -> BABYDEER). Kept sorted: inCatalog binary-searches.
var DayzFlags = []string{
	"ALTIS", "APA", "BABYDEER", "BEAR", "BOHEMIA", "BRAINZ", "CANNIBALS", "CDF", "CHEDAKI", "CHEL", "CHERNARUS", "CMC",
	"CROOK", "DAYZ", "HUNTERZ", "LIVONIA", "LIVONIAARMY", "LIVONIAPOLICE", "NAPA", "NSAHRANI", "PIRATES", "REFUGE",
	"REX", "ROOSTER", "RSTA", "SNAKE", "SSAHRANI", "TEC", "UEC", "WHITE", "WOLF", "ZAGORKY", "ZENIT",
}

// FlagClassName returns the DayZ item class name for an approved flag key ("CDF" -> "Flag_CDF").
// The mixed-case spellings match the game's own class list so server files line up.
func FlagClassName(key string) (string, bool) {
	if !inCatalog(DayzFlags, key) {
		return "", false
	}
	return "Flag_" + flagClassSpelling[key], true
}

var flagClassSpelling = map[string]string{
	"ALTIS": "Altis", "APA": "APA", "BABYDEER": "BabyDeer", "BEAR": "Bear", "BOHEMIA": "Bohemia", "BRAINZ": "BrainZ",
	"CANNIBALS": "Cannibals", "CDF": "CDF", "CHEDAKI": "Chedaki", "CHEL": "CHEL", "CHERNARUS": "Chernarus", "CMC": "CMC",
	"CROOK": "Crook", "DAYZ": "DayZ", "HUNTERZ": "HunterZ", "LIVONIA": "Livonia", "LIVONIAARMY": "LivoniaArmy",
	"LIVONIAPOLICE": "LivoniaPolice", "NAPA": "NAPA", "NSAHRANI": "NSahrani", "PIRATES": "Pirates", "REFUGE": "Refuge",
	"REX": "Rex", "ROOSTER": "Rooster", "RSTA": "RSTA", "SNAKE": "Snake", "SSAHRANI": "SSahrani", "TEC": "TEC", "UEC": "UEC",
	"WHITE": "White", "WOLF": "Wolf", "ZAGORKY": "Zagorky", "ZENIT": "Zenit",
}

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

// BrandingTakenError: the flag or armband is already claimed by another faction on the
// installation. Flags and armbands are exclusive per server (first come, first served); the
// holder is named so the picker can show who has it.
type BrandingTakenError struct {
	Field      string // "flagKey" or "armbandKey"
	Key        string
	HolderID   int64
	HolderName string
	HolderTag  string
}

func (e *BrandingTakenError) Error() string {
	what := "flag"
	if e.Field == "armbandKey" {
		what = "armband"
	}
	if e.HolderTag == "" {
		return "that " + what + " is already claimed by another faction on this server"
	}
	return "that " + what + " is already claimed by " + e.HolderName + " [" + e.HolderTag + "]"
}

// Phase 4 errors.
var (
	// ErrLeadershipTransferRequired: the LEADER cannot leave while still leader.
	ErrLeadershipTransferRequired = errors.New("the leader must transfer leadership before leaving")
	// ErrAlreadyLeader: the transfer target already leads the faction.
	ErrAlreadyLeader = errors.New("the member is already the leader")
)
