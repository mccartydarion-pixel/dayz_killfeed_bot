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
}
