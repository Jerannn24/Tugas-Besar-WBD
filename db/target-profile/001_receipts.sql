-- Profil awal target a atau b, dijalankan melalui psql.
-- Gunakan akun bootstrap pada target baru; siapkan kredensial secara terpisah.
\getenv target_slot TARGET_SLOT
\getenv executor_password TARGET_EXECUTOR_PASSWORD
\getenv preparer_password TARGET_PREPARER_PASSWORD
\getenv viewer_password TARGET_VIEWER_PASSWORD
SELECT :'target_slot' IN ('a', 'b') AS target_slot_ok \gset
\if :target_slot_ok
\else
\echo 'TARGET_SLOT must be a or b'
\quit 3
\endif

SELECT NOT EXISTS (SELECT FROM pg_catalog.pg_namespace WHERE nspname IN ('wbd_auth', 'managed'))
       AND NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname IN
         ('wbd_target_' || :'target_slot' || '_executor', 'wbd_target_' || :'target_slot' || '_preparer',
          'wbd_target_' || :'target_slot' || '_viewer', 'wbd_target_' || :'target_slot' || '_receipt_owner')) AS profile_available \gset
\if :profile_available
\else
\echo 'Target schemas or roles already exist; refusing takeover'
SELECT 1 / 0;
\endif

SELECT pg_catalog.format('CREATE ROLE %I NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS', 'wbd_target_' || :'target_slot' || '_receipt_owner') \gexec
SELECT pg_catalog.format('CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS', 'wbd_target_' || :'target_slot' || '_executor') \gexec
SELECT pg_catalog.format('CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS', 'wbd_target_' || :'target_slot' || '_preparer') \gexec
SELECT pg_catalog.format('CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS', 'wbd_target_' || :'target_slot' || '_viewer') \gexec
SELECT pg_catalog.format('ALTER ROLE %I PASSWORD %L', 'wbd_target_' || :'target_slot' || '_executor', :'executor_password') \gexec
SELECT pg_catalog.format('ALTER ROLE %I PASSWORD %L', 'wbd_target_' || :'target_slot' || '_preparer', :'preparer_password') \gexec
SELECT pg_catalog.format('ALTER ROLE %I PASSWORD %L', 'wbd_target_' || :'target_slot' || '_viewer', :'viewer_password') \gexec
SELECT pg_catalog.format('ALTER ROLE %I SET search_path = pg_catalog, managed, pg_temp', 'wbd_target_' || :'target_slot' || '_executor') \gexec
SELECT pg_catalog.format('ALTER ROLE %I SET standard_conforming_strings = on', 'wbd_target_' || :'target_slot' || '_executor') \gexec
SELECT pg_catalog.format('REVOKE CONNECT, TEMPORARY ON DATABASE %I FROM PUBLIC', pg_catalog.current_database()) \gexec
SELECT pg_catalog.format('GRANT CONNECT ON DATABASE %I TO %I, %I, %I', pg_catalog.current_database(), 'wbd_target_' || :'target_slot' || '_executor', 'wbd_target_' || :'target_slot' || '_preparer', 'wbd_target_' || :'target_slot' || '_viewer') \gexec

CREATE SCHEMA wbd_auth;
SELECT pg_catalog.format('ALTER SCHEMA wbd_auth OWNER TO %I', 'wbd_target_' || :'target_slot' || '_receipt_owner') \gexec
REVOKE ALL ON SCHEMA wbd_auth FROM PUBLIC;
SELECT pg_catalog.format('GRANT USAGE ON SCHEMA wbd_auth TO %I, %I', 'wbd_target_' || :'target_slot' || '_executor', 'wbd_target_' || :'target_slot' || '_preparer') \gexec
SELECT pg_catalog.format('CREATE SCHEMA managed AUTHORIZATION %I', 'wbd_target_' || :'target_slot' || '_executor') \gexec
REVOKE ALL ON SCHEMA managed FROM PUBLIC;
SELECT pg_catalog.format('GRANT USAGE ON SCHEMA managed TO %I', 'wbd_target_' || :'target_slot' || '_viewer') \gexec
SELECT pg_catalog.format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA managed GRANT SELECT ON TABLES TO %I', 'wbd_target_' || :'target_slot' || '_executor', 'wbd_target_' || :'target_slot' || '_viewer') \gexec
CREATE TABLE wbd_auth.profile_migrations (
  version text PRIMARY KEY,
  checksum text NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
  applied_at timestamptz NOT NULL DEFAULT pg_catalog.clock_timestamp()
);

CREATE TABLE wbd_auth.target_marker (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  physical_target_id uuid NOT NULL UNIQUE DEFAULT pg_catalog.gen_random_uuid(),
  executor_role name NOT NULL,
  created_at timestamptz NOT NULL DEFAULT pg_catalog.clock_timestamp()
);
CREATE TABLE wbd_auth.execution_tickets (
  execution_id uuid PRIMARY KEY,
  physical_target_id uuid NOT NULL REFERENCES wbd_auth.target_marker(physical_target_id),
  database_id uuid NOT NULL,
  target_xid xid8 NOT NULL,
  version bigint NOT NULL CHECK (version > 0),
  sql_sha256 bytea NOT NULL CHECK (pg_catalog.octet_length(sql_sha256) = 32),
  actor_id uuid NOT NULL,
  capability_sha256 bytea NOT NULL CHECK (pg_catalog.octet_length(capability_sha256) = 32),
  prepared_at timestamptz NOT NULL DEFAULT pg_catalog.clock_timestamp(),
  UNIQUE (physical_target_id, version)
);
CREATE TABLE wbd_auth.execution_receipts (
  execution_id uuid PRIMARY KEY,
  physical_target_id uuid NOT NULL REFERENCES wbd_auth.target_marker(physical_target_id),
  database_id uuid NOT NULL,
  target_xid xid8 NOT NULL,
  version bigint NOT NULL CHECK (version > 0),
  sql_sha256 bytea NOT NULL CHECK (pg_catalog.octet_length(sql_sha256) = 32),
  actor_id uuid NOT NULL,
  UNIQUE (physical_target_id, version),
  recorded_at timestamptz NOT NULL DEFAULT pg_catalog.clock_timestamp()
);
SELECT pg_catalog.format('ALTER TABLE wbd_auth.%I OWNER TO %I', t, 'wbd_target_' || :'target_slot' || '_receipt_owner')
FROM (VALUES ('target_marker'), ('execution_tickets'), ('execution_receipts'), ('profile_migrations')) AS x(t) \gexec
INSERT INTO wbd_auth.target_marker(executor_role)
VALUES ( 'wbd_target_' || :'target_slot' || '_executor');

CREATE FUNCTION wbd_auth.record_receipt(p_execution_id uuid, p_capability bytea)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $function$
DECLARE
  ticket wbd_auth.execution_tickets%ROWTYPE;
  marker wbd_auth.target_marker%ROWTYPE;
BEGIN
  SELECT * INTO STRICT marker FROM wbd_auth.target_marker WHERE singleton;
  IF session_user <> marker.executor_role::text OR p_capability IS NULL OR pg_catalog.octet_length(p_capability) <> 32 THEN
    RAISE EXCEPTION 'execution receipt authorization rejected' USING ERRCODE = '42501';
  END IF;
  SELECT * INTO STRICT ticket FROM wbd_auth.execution_tickets WHERE execution_id = p_execution_id;
  IF ticket.target_xid <> pg_catalog.pg_current_xact_id()
     OR ticket.physical_target_id <> marker.physical_target_id
     OR ticket.capability_sha256 <> pg_catalog.sha256(p_capability) THEN
    RAISE EXCEPTION 'execution receipt authorization rejected' USING ERRCODE = '42501';
  END IF;
  INSERT INTO wbd_auth.execution_receipts
    (execution_id, physical_target_id, database_id, target_xid, version, sql_sha256, actor_id)
  VALUES (ticket.execution_id, ticket.physical_target_id, ticket.database_id,
          ticket.target_xid, ticket.version, ticket.sql_sha256, ticket.actor_id);
END
$function$;
SELECT pg_catalog.format('ALTER FUNCTION wbd_auth.record_receipt(uuid, bytea) OWNER TO %I', 'wbd_target_' || :'target_slot' || '_receipt_owner') \gexec
REVOKE ALL ON FUNCTION wbd_auth.record_receipt(uuid, bytea) FROM PUBLIC;
SELECT pg_catalog.format('GRANT EXECUTE ON FUNCTION wbd_auth.record_receipt(uuid, bytea) TO %I', 'wbd_target_' || :'target_slot' || '_executor') \gexec
SELECT pg_catalog.format('GRANT INSERT ON wbd_auth.execution_tickets TO %I', 'wbd_target_' || :'target_slot' || '_preparer') \gexec
SELECT pg_catalog.format('GRANT SELECT ON wbd_auth.target_marker TO %I', 'wbd_target_' || :'target_slot' || '_preparer') \gexec
SELECT pg_catalog.format('GRANT SELECT ON wbd_auth.execution_receipts TO %I', 'wbd_target_' || :'target_slot' || '_preparer') \gexec
REVOKE ALL ON ALL TABLES IN SCHEMA wbd_auth FROM PUBLIC;
