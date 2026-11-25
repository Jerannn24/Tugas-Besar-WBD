-- Profil batch compensation target a atau b, dijalankan melalui psql.
-- Dijalankan setelah 001_receipts.sql, 002_batch_checkpoints.sql, 003_batch_scopes.sql, dan 004_batch_ticket_reservation.sql.
\getenv target_slot TARGET_SLOT
SELECT :'target_slot' IN ('a', 'b') AS target_slot_ok \gset
\if :target_slot_ok
\else
\echo 'TARGET_SLOT must be a or b'
\quit 3
\endif

SELECT EXISTS (SELECT FROM pg_catalog.pg_namespace WHERE nspname = 'wbd_auth')
       AND EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'target_marker')
       AND EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'batch_tickets')
       AND EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'batch_checkpoints')
       AND EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'batch_scopes')
       AND EXISTS (SELECT FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'wbd_auth' AND p.proname = 'reserve_batch_ticket')
       AND NOT EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'batch_row_versions') AS profile_005_ready \gset
\if :profile_005_ready
\else
\echo 'Prerequisite profile 001/002/003/004 missing or batch_row_versions already exists'
SELECT 1 / 0;
\endif

CREATE TABLE wbd_auth.batch_row_versions (
  execution_id uuid NOT NULL,
  physical_target_id uuid NOT NULL REFERENCES wbd_auth.target_marker(physical_target_id),
  batch_index integer NOT NULL CHECK (batch_index >= 0),
  item_id bigint NOT NULL,
  target_xid xid8 NOT NULL,
  row_xmin text NOT NULL,
  row_ctid text NOT NULL,
  recorded_at timestamptz NOT NULL DEFAULT pg_catalog.clock_timestamp(),
  PRIMARY KEY (execution_id, batch_index, item_id)
);

SELECT pg_catalog.format('ALTER TABLE wbd_auth.batch_row_versions OWNER TO %I', 'wbd_target_' || :'target_slot' || '_receipt_owner') \gexec
REVOKE ALL ON TABLE wbd_auth.batch_row_versions FROM PUBLIC;
SELECT pg_catalog.format('GRANT SELECT ON TABLE wbd_auth.batch_row_versions TO %I', 'wbd_target_' || :'target_slot' || '_preparer') \gexec
SELECT pg_catalog.format('GRANT SELECT, INSERT ON TABLE wbd_auth.batch_row_versions TO %I', 'wbd_target_' || :'target_slot' || '_executor') \gexec
