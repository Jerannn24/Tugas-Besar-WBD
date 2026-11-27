\set ON_ERROR_STOP on
SET client_min_messages = warning;
\if :{?rows}
\else
\set rows 1000000
\endif
\if :{?expected_db}
\else
\echo 'batch_items: set -v expected_db=<target database>'
SELECT 1 / 0;
\endif

SELECT current_database() = :'expected_db'
       AND current_user ~ '^wbd_target_[ab]_executor$'
       AND session_user = current_user
       AND current_setting('server_version_num')::int BETWEEN 160000 AND 169999
       AND :'rows' ~ '^[1-9][0-9]{0,8}$' AS guard_ok \gset
\if :guard_ok
\else
\echo 'batch_items: unexpected database, role, server version, or rows'
SELECT 1 / 0;
\endif

CREATE TABLE IF NOT EXISTS managed.batch_items (
  id bigint PRIMARY KEY,
  processed boolean NOT NULL
);

SELECT (SELECT array_agg(attname::text || ':' || format_type(atttypid, atttypmod) || ':' || attnotnull::text ORDER BY attnum)
          FROM pg_attribute
         WHERE attrelid = 'managed.batch_items'::regclass AND attnum > 0 AND NOT attisdropped)
         = ARRAY['id:bigint:true', 'processed:boolean:true']
       AND EXISTS (SELECT FROM pg_constraint
                    WHERE conrelid = 'managed.batch_items'::regclass AND contype = 'p' AND conkey = ARRAY[1]::smallint[])
       AND pg_get_userbyid((SELECT relowner FROM pg_class WHERE oid = 'managed.batch_items'::regclass)) = current_user
       AS shape_ok \gset
\if :shape_ok
\else
\echo 'batch_items: existing table has unexpected columns, key, or owner; refusing to touch it'
SELECT 1 / 0;
\endif

SELECT pg_catalog.format('GRANT SELECT (id, processed) ON managed.batch_items TO %I', regexp_replace(current_user, '_executor$', '_receipt_owner')) \gexec
