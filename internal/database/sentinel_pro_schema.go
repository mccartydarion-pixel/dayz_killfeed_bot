package database

// SentinelProSQL lets the Security Marketplace sell the Sentinel Pro bundle:
// paid time for it counts for every base service it covers. Widens the two
// service_id checks; no new tables.
const SentinelProSQL = `
ALTER TABLE security_service_offers DROP CONSTRAINT IF EXISTS security_service_offers_service_id_check;
ALTER TABLE security_service_offers ADD CONSTRAINT security_service_offers_service_id_check
 CHECK (service_id IN ('BASE_RAID_ALARM','PERIMETER_MONITORING','BASE_BLACK_BOX','FACTION_SECURITY','SENTINEL_PRO'));
ALTER TABLE security_service_purchases DROP CONSTRAINT IF EXISTS security_service_purchases_service_id_check;
ALTER TABLE security_service_purchases ADD CONSTRAINT security_service_purchases_service_id_check
 CHECK (service_id IN ('BASE_RAID_ALARM','PERIMETER_MONITORING','BASE_BLACK_BOX','FACTION_SECURITY','SENTINEL_PRO'));
`
