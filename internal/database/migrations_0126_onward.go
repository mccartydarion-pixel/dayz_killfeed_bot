package database

// migrations0126onward holds migrations 0126 and later, in execution order. It is one fragment of
// the registry assembled in migrations.go; never edit an applied migration.
var migrations0126onward = []Migration{
	{
		// Interactive reads (interactive_reads_schema.go): a partial index for "who is online on
		// this server". One small index on a small table; additive.
		Name: "0126_interactive_read_indexes",
		SQL:  InteractiveReadIndexesSQL,
	},
	{
		// Map rotation: fresh characters on every map switch (docs/MAP_ROTATION.md). One owner
		// option (default off), the time of Champion's own restart, and the progress and outcome of
		// clearing the characters on a switch. Additive.
		Name: "0127_map_rotation_wipe_characters",
		SQL:  MapRotationWipeCharactersSQL,
	},
	{
		// Map rotation: a map's picture is uploaded and stored in Champion instead of linked
		// (docs/MAP_ROTATION.md). Three nullable columns on map_rotation_maps. Additive.
		Name: "0128_map_rotation_map_images",
		SQL:  MapRotationMapImagesSQL,
	},
	{
		// Ranked seasons: the wait before the same attacker earns RP from the same victim again
		// becomes part of a season's frozen rules (docs/RANKED_SERVER_SEASONS.md). Existing
		// seasons, active and archived, keep the old fixed five minutes. Additive.
		Name: "0129_ranked_same_victim_cooldown",
		SQL:  RankedSameVictimCooldownSQL,
	},
	{
		// Ranked seasons: the same-victim wait can change during a season
		// (docs/RANKED_SERVER_SEASONS.md). The history of values lets the award path use the wait
		// that was in force when a kill happened. Existing seasons are seeded from their own row,
		// effective from the season start. Additive.
		Name: "0130_ranked_cooldown_changes",
		SQL:  RankedCooldownChangesSQL,
	},
	{
		// The Stadium (docs/STADIUM.md): the owner's arena configuration and its build state, one
		// row per installation. Additive.
		Name: "0131_stadiums",
		SQL:  StadiumSQL,
	},
	{
		// Tournament mode (docs/TOURNAMENTS.md): tournaments, entries, the bracket's matches,
		// rounds, the champion's title and the prize payouts. Additive.
		Name: "0132_tournaments",
		SQL:  TournamentSQL,
	},
	{
		// Self-serve organization erase (docs/SAAS_HTTP_API.md "Erase an organization"): payment
		// records and the one-trial-per-account marker outlive the organization they belonged to.
		// Both references become nullable ON DELETE SET NULL instead of CASCADE. No data changes.
		Name: "0133_erase_keeps_billing_records",
		SQL:  EraseKeepsBillingRecordsSQL,
	},
}

// EraseKeepsBillingRecordsSQL keeps billing_transactions (needed for tax and accounting) and
// trial_grants (one no-card trial per Discord account) when their organization is deleted: the
// organization_id is cleared instead of the row being removed.
const EraseKeepsBillingRecordsSQL = `
ALTER TABLE billing_transactions ALTER COLUMN organization_id DROP NOT NULL;
ALTER TABLE billing_transactions DROP CONSTRAINT IF EXISTS billing_transactions_organization_id_fkey;
ALTER TABLE billing_transactions ADD CONSTRAINT billing_transactions_organization_id_fkey
    FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE SET NULL;
ALTER TABLE trial_grants ALTER COLUMN organization_id DROP NOT NULL;
ALTER TABLE trial_grants DROP CONSTRAINT IF EXISTS trial_grants_organization_id_fkey;
ALTER TABLE trial_grants ADD CONSTRAINT trial_grants_organization_id_fkey
    FOREIGN KEY (organization_id) REFERENCES organizations(id) ON DELETE SET NULL;
`
