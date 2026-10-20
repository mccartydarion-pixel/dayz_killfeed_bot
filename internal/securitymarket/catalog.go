package securitymarket

import "time"

// Security services are player purchases, not C.A.S.E. anti-cheat detectors.
// Only the Base Raid Alarm can be sold, and only when the server owner turns
// the alarm on and offers it for Champion Points. The rest stay unavailable.
type Service struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	RequiredCapability string `json:"requiredCapability"`
}

type Availability struct {
	Service     Service `json:"service"`
	Status      string  `json:"status"`
	Reason      string  `json:"reason"`
	Purchasable bool    `json:"purchasable"`
	// Set only for a service the server owner is selling.
	PricePoints  int64      `json:"pricePoints,omitempty"`
	DurationDays int        `json:"durationDays,omitempty"`
	ActiveUntil  *time.Time `json:"activeUntil,omitempty"`
	// Includes lists, for a bundle, the services it covers that are switched on.
	Includes []string `json:"includes,omitempty"`
}

var definitions = []Service{
	{"BASE_BOOST_ALERT", "C.A.S.E. Base Boost Alert", "VERIFIED_BASE_BUILD_EVENTS"},
	{"BASE_RAID_ALARM", "Base Raid Alarm", "VERIFIED_BASE_RAID_EVENTS"},
	{"BASE_BLACK_BOX", "Base Black Box", "VERIFIED_BASE_EVENT_HISTORY"},
	{"OFFLINE_PROTECTION", "Offline Protection", "VERIFIED_OFFLINE_PROTECTION_CONTROL"},
	{"PERIMETER_MONITORING", "Perimeter Monitoring", "VERIFIED_PERIMETER_EVENTS"},
	{"FACTION_SECURITY", "Faction Security", "VERIFIED_FACTION_PERMISSIONS_AND_EVENTS"},
	{"SENTINEL_PRO", "Sentinel Pro", "VERIFIED_BUNDLED_SERVICE_CAPABILITIES"},
}

// ProposedCatalog is a defensive copy so a caller cannot mutate product
// definitions. An owner setting a price must not override source eligibility.
func ProposedCatalog() []Service {
	out := make([]Service, len(definitions))
	copy(out, definitions)
	return out
}

// UnverifiedCatalog marks every service unavailable. The app layer then opens
// the Base Raid Alarm when the owner sells it (see saas_api_security_marketplace.go).
// Enabling a shop item or selecting a sensitivity does not verify telemetry.
func UnverifiedCatalog() []Availability {
	out := make([]Availability, 0, len(definitions))
	for _, service := range definitions {
		out = append(out, Availability{Service: service, Status: "UNSUPPORTED",
			Reason: "REQUIRED_CAPABILITY_NOT_VERIFIED", Purchasable: false})
	}
	return out
}
