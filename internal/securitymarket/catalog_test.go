package securitymarket

import "testing"

func TestUnverifiedSecurityServicesCannotBecomeShopOffers(t *testing.T) {
	catalog := UnverifiedCatalog()
	if len(catalog) != 7 {
		t.Fatalf("unexpected security-service count: %d", len(catalog))
	}
	ids := map[string]bool{}
	for _, item := range catalog {
		if item.Purchasable || item.Status != "UNSUPPORTED" || item.Reason != "REQUIRED_CAPABILITY_NOT_VERIFIED" ||
			item.Service.ID == "" || item.Service.Name == "" || item.Service.RequiredCapability == "" {
			t.Fatalf("unverified service appeared available: %+v", item)
		}
		if ids[item.Service.ID] {
			t.Fatalf("duplicate service ID: %s", item.Service.ID)
		}
		ids[item.Service.ID] = true
	}
	catalog[0].Service.Name = "changed"
	if ProposedCatalog()[0].Name == "changed" {
		t.Fatal("catalog mutation escaped defensive copy")
	}
}
