-- Profil batch checkpoint target a atau b, dijalankan melalui psql.
-- Dijalankan setelah 001_receipts.sql.
\getenv target_slot TARGET_SLOT
SELECT :'target_slot' IN ('a', 'b') AS target_slot_ok \gset
\if :target_slot_ok
\else
\echo 'TARGET_SLOT must be a or b'
\quit 3
\endif

SELECT EXISTS (SELECT FROM pg_catalog.pg_namespace WHERE nspname = 'wbd_auth')
       AND EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'target_marker')
       AND NOT EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'batch_checkpoints') AS profile_002_ready \gset
\if :profile_002_ready
\else
\echo 'Prerequisite profile 001 missing or batch_checkpoints already exists'
SELECT 1 / 0;
\endif

CREATE TABLE wbd_auth.batch_tickets (
  execution_id uuid NOT NULL,
  physical_target_id uuid NOT NULL REFERENCES wbd_auth.target_marker(physical_target_id),
  target_xid xid8 NOT NULL,
  batch_index integer NOT NULL CHECK (batch_index >= 0),
  direction text NOT NULL CHECK (direction IN ('up', 'down')),
  capability_sha256 bytea NOT NULL CHECK (pg_catalog.octet_length(capability_sha256) = 32),
  PRIMARY KEY (execution_id, direction, batch_index)
);
SELECT pg_catalog.format('ALTER TABLE wbd_auth.batch_tickets OWNER TO %I', 'wbd_target_' || :'target_slot' || '_receipt_owner') \gexec
REVOKE ALL ON TABLE wbd_auth.batch_tickets FROM PUBLIC;
SELECT pg_catalog.format('GRANT INSERT ON TABLE wbd_auth.batch_tickets TO %I', 'wbd_target_' || :'target_slot' || '_preparer') \gexec

CREATE TABLE wbd_auth.batch_checkpoints (
  execution_id uuid NOT NULL,
  physical_target_id uuid NOT NULL REFERENCES wbd_auth.target_marker(physical_target_id),
  target_xid xid8 NOT NULL,
  batch_index integer NOT NULL CHECK (batch_index >= 0),
  direction text NOT NULL CHECK (direction IN ('up', 'down')),
  rows_affected bigint NOT NULL CHECK (rows_affected >= 0),
  recorded_at timestamptz NOT NULL DEFAULT pg_catalog.clock_timestamp(),
  PRIMARY KEY (execution_id, direction, batch_index)
);

SELECT pg_catalog.format('ALTER TABLE wbd_auth.batch_checkpoints OWNER TO %I', 'wbd_target_' || :'target_slot' || '_receipt_owner') \gexec
REVOKE ALL ON TABLE wbd_auth.batch_checkpoints FROM PUBLIC;
SELECT pg_catalog.format('GRANT SELECT ON TABLE wbd_auth.batch_checkpoints TO %I', 'wbd_target_' || :'target_slot' || '_preparer') \gexec

CREATE FUNCTION wbd_auth.record_checkpoint(
  p_execution_id uuid,
  p_batch_index integer,
  p_direction text,
  p_rows_affected bigint,
  p_capability bytea
)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $function$
DECLARE
  marker wbd_auth.target_marker%ROWTYPE;
  ticket wbd_auth.batch_tickets%ROWTYPE;
BEGIN
  SELECT * INTO STRICT marker FROM wbd_auth.target_marker WHERE singleton;
  IF session_user <> marker.executor_role::text OR p_capability IS NULL
     OR pg_catalog.octet_length(p_capability) <> 32 THEN
    RAISE EXCEPTION 'checkpoint authorization rejected' USING ERRCODE = '42501';
  END IF;
  IF p_execution_id IS NULL OR p_batch_index IS NULL OR p_batch_index < 0
     OR p_direction IS NULL OR p_direction NOT IN ('up', 'down')
     OR p_rows_affected IS NULL OR p_rows_affected < 0 THEN
    RAISE EXCEPTION 'invalid checkpoint parameters' USING ERRCODE = '22023';
  END IF;
  SELECT * INTO ticket FROM wbd_auth.batch_tickets
    WHERE execution_id = p_execution_id AND batch_index = p_batch_index AND direction = p_direction;
  IF NOT FOUND OR ticket.target_xid <> pg_catalog.pg_current_xact_id()
     OR ticket.physical_target_id <> marker.physical_target_id
     OR ticket.capability_sha256 <> pg_catalog.sha256(p_capability) THEN
    RAISE EXCEPTION 'checkpoint authorization rejected' USING ERRCODE = '42501';
  END IF;
  INSERT INTO wbd_auth.batch_checkpoints
    (execution_id, physical_target_id, target_xid, batch_index, direction, rows_affected)
  VALUES (ticket.execution_id, ticket.physical_target_id, ticket.target_xid,
          ticket.batch_index, ticket.direction, p_rows_affected);
END
$function$;

SELECT pg_catalog.format('ALTER FUNCTION wbd_auth.record_checkpoint(uuid, integer, text, bigint, bytea) OWNER TO %I', 'wbd_target_' || :'target_slot' || '_receipt_owner') \gexec
REVOKE ALL ON FUNCTION wbd_auth.record_checkpoint(uuid, integer, text, bigint, bytea) FROM PUBLIC;
SELECT pg_catalog.format('GRANT EXECUTE ON FUNCTION wbd_auth.record_checkpoint(uuid, integer, text, bigint, bytea) TO %I', 'wbd_target_' || :'target_slot' || '_executor') \gexec
