package securitymarket

// Security services are player purchases, not C.A.S.E. anti-cheat detectors.
// Catalog entries describe proposed products only. No service is purchasable
// until a protected, installation-scoped capability verifier is wired in.
type Service struct {
 ID string `json:"id"`
 Name string `json:"name"`
 RequiredCapability string `json:"requiredCapability"`
}

type Availability struct {
 Service Service `json:"service"`
 Status string `json:"status"`
 Reason string `json:"reason"`
 Purchasable bool `json:"purchasable"`
}

var definitions=[]Service{
 {"BASE_BOOST_ALERT","C.A.S.E. Base Boost Alert","VERIFIED_BASE_BUILD_EVENTS"},
 {"BASE_RAID_ALARM","Base Raid Alarm","VERIFIED_BASE_RAID_EVENTS"},
 {"BASE_BLACK_BOX","Base Black Box","VERIFIED_BASE_EVENT_HISTORY"},
 {"OFFLINE_PROTECTION","Offline Protection","VERIFIED_OFFLINE_PROTECTION_CONTROL"},
 {"PERIMETER_MONITORING","Perimeter Monitoring","VERIFIED_PERIMETER_EVENTS"},
 {"FACTION_SECURITY","Faction Security","VERIFIED_FACTION_PERMISSIONS_AND_EVENTS"},
 {"SENTINEL_PRO","Sentinel Pro","VERIFIED_BUNDLED_SERVICE_CAPABILITIES"},
}

// ProposedCatalog is a defensive copy so a caller cannot mutate product
// definitions. An owner setting a price must not override source eligibility.
func ProposedCatalog() []Service {
 out:=make([]Service,len(definitions))
 copy(out,definitions)
 return out
}

// UnverifiedCatalog is the only current offer projection. In particular,
// enabling a shop item or selecting a sensitivity does not verify telemetry.
func UnverifiedCatalog() []Availability {
 out:=make([]Availability,0,len(definitions))
 for _,service:=range definitions {
  out=append(out,Availability{Service:service,Status:"UNSUPPORTED",
   Reason:"REQUIRED_CAPABILITY_NOT_VERIFIED",Purchasable:false})
 }
 return out
}
