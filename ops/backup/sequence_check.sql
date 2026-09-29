-- Restored-copy sequence safety check (docs/PRODUCTION_BACKUP.md). Run against the DISPOSABLE restored
-- database only. For every sequence owned by a column (serial or identity), the column's current
-- maximum must not be above the sequence's position; otherwise the first insert after a recovery could
-- reuse an existing id. Prints one line per violation ("sequence_behind_data|<schema>.<sequence>"),
-- nothing when every sequence is safe. Reads only.
\set ON_ERROR_STOP 1
\pset format unaligned
\pset tuples_only on
\pset footer off
SELECT format(
    'SELECT %L WHERE (SELECT max(%I) FROM %I.%I) > (SELECT CASE WHEN is_called THEN last_value ELSE last_value - 1 END FROM %I.%I)',
    'sequence_behind_data|' || sn.nspname || '.' || s.relname,
    a.attname, tn.nspname, t.relname, sn.nspname, s.relname)
FROM pg_depend d
JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
JOIN pg_namespace sn ON sn.oid = s.relnamespace
JOIN pg_class t ON t.oid = d.refobjid AND t.relkind IN ('r', 'p')
JOIN pg_namespace tn ON tn.oid = t.relnamespace
JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = d.refobjsubid
WHERE d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_class'::regclass AND d.deptype IN ('a', 'i')
  AND sn.nspname NOT IN ('pg_catalog', 'information_schema')
ORDER BY 1
\gexec
