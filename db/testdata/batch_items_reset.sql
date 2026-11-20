\set ON_ERROR_STOP on
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
       AND :'rows' ~ '^[1-9][0-9]{0,8}$'
       AND to_regclass('managed.batch_items') IS NOT NULL AS guard_ok \gset
\if :guard_ok
\else
\echo 'batch_items: unexpected database, role, server version, rows, or missing table (run batch_items_seed.sql first)'
SELECT 1 / 0;
\endif

SELECT clock_timestamp() AS reset_started \gset
BEGIN;

TRUNCATE managed.batch_items;
INSERT INTO managed.batch_items (id, processed)
SELECT g, false FROM generate_series(1, :rows::bigint) AS g;
COMMIT;
VACUUM (ANALYZE) managed.batch_items;
SELECT round(extract(epoch FROM clock_timestamp() - :'reset_started'::timestamptz) * 1000, 1) AS reset_ms \gset

SELECT count(*) = :rows::bigint
       AND count(*) FILTER (WHERE processed) = 0
       AND min(id) = 1
       AND max(id) = :rows::bigint AS reset_ok
FROM managed.batch_items \gset
\if :reset_ok
\echo RESULT reset_ms=:reset_ms rows=:rows
\else
\echo 'batch_items: reset verification failed'
SELECT 1 / 0;
\endif
