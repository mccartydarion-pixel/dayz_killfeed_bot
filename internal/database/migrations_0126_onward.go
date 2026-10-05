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
}
