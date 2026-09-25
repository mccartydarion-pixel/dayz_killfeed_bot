-- Database inventory used to prove a backup restores exactly (docs/PRODUCTION_BACKUP.md).
-- Run with psql inside the transaction/snapshot being described. Output: one line per fact, sorted.
-- It reads only; it never prints credentials. The output contains per-table row counts and content
-- hashes (not row data) and is kept only inside the encrypted backup bundle.
\set ON_ERROR_STOP 1
\pset format unaligned
\pset tuples_only on
\pset footer off
SET TimeZone = 'UTC';
SET extra_float_digits = 1;

SELECT 'migration|' || name FROM schema_migrations ORDER BY name;

SELECT 'object|' || c.relkind::text || '|' || n.nspname || '.' || c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%'
  AND c.relkind IN ('r', 'p', 'i', 'S', 'v', 'm', 'f')
ORDER BY 1;

SELECT 'trigger|' || n.nspname || '.' || c.relname || '|' || t.tgname || '|' || t.tgenabled::text
FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE NOT t.tgisinternal AND n.nspname NOT IN ('pg_catalog', 'information_schema')
ORDER BY 1;

SELECT 'function|' || n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')|' || md5(pg_get_functiondef(p.oid))
FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
LEFT JOIN pg_depend d ON d.objid = p.oid AND d.deptype = 'e'
WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND p.prokind IN ('f', 'p') AND d.objid IS NULL
ORDER BY 1;

SELECT 'constraint|' || n.nspname || '.' || conrelid::regclass::text || '|' || conname || '|' || md5(pg_get_constraintdef(c.oid))
FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace
WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND c.conrelid <> 0
ORDER BY 1;

SELECT 'index|' || schemaname || '.' || indexname || '|' || md5(indexdef)
FROM pg_indexes WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
ORDER BY 1;

SELECT 'extension|' || extname || '|' || extversion FROM pg_extension ORDER BY 1;

SELECT 'sequence|' || schemaname || '.' || sequencename || '|' || COALESCE(last_value::text, 'unset')
FROM pg_sequences WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
ORDER BY 1;

-- Per table: exact row count and a content hash over every row's full text form in byte order.
-- ROW(alias.*) always means the whole row, even when a column shares the alias name.
SELECT format(
    'SELECT %L || ''|'' || COUNT(*) || ''|'' || COALESCE(md5(string_agg(ROW(inventory_row.*)::text, E''\n'' ORDER BY ROW(inventory_row.*)::text COLLATE "C")), ''empty'') FROM %I.%I inventory_row',
    'table|' || n.nspname || '.' || c.relname, n.nspname, c.relname)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition
  AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_toast%'
ORDER BY n.nspname, c.relname COLLATE "C"
\gexec
