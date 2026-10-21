package database

// FactionBrandingExclusiveSQL makes a faction's DayZ flag and armband exclusive per installation
// (first come, first served; docs/FACTIONS.md "Branding"). The flag catalog also changed from four
// placeholder colours to the 33 flags the game ships, so every stored flag_key outside the new
// catalog is cleared. Where two factions on one installation already held the same armband, the
// older claim (lower id) wins and the later one is cleared, so the unique indexes can be built.
const FactionBrandingExclusiveSQL = `
UPDATE hub_factions SET flag_key = NULL, updated_at = NOW()
WHERE flag_key IS NOT NULL AND flag_key NOT IN (
  'ALTIS','APA','BABYDEER','BEAR','BOHEMIA','BRAINZ','CANNIBALS','CDF','CHEDAKI','CHEL','CHERNARUS','CMC','CROOK','DAYZ',
  'HUNTERZ','LIVONIA','LIVONIAARMY','LIVONIAPOLICE','NAPA','NSAHRANI','PIRATES','REFUGE','REX','ROOSTER','RSTA','SNAKE',
  'SSAHRANI','TEC','UEC','WHITE','WOLF','ZAGORKY','ZENIT');

UPDATE hub_factions f SET flag_key = NULL, updated_at = NOW()
WHERE f.flag_key IS NOT NULL
  AND EXISTS (SELECT 1 FROM hub_factions g WHERE g.installation_id = f.installation_id AND g.flag_key = f.flag_key AND g.id < f.id);

UPDATE hub_factions f SET armband_key = NULL, updated_at = NOW()
WHERE f.armband_key IS NOT NULL
  AND EXISTS (SELECT 1 FROM hub_factions g WHERE g.installation_id = f.installation_id AND g.armband_key = f.armband_key AND g.id < f.id);

CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_factions_installation_flag ON hub_factions(installation_id, flag_key) WHERE flag_key IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_factions_installation_armband ON hub_factions(installation_id, armband_key) WHERE armband_key IS NOT NULL;
`
