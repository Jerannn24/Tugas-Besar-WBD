package connections

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// unverified reports a failed verification. Only fixed reason strings are
// used so that no DSN, host detail or driver error text can leak.
func unverified(reason string) error { return fmt.Errorf("%w: %s", ErrTargetUnavailable, reason) }

type sessionFacts struct {
	Database, SessionUser, Encoding string
	Version                         int
	Recovery, Unsafe                bool
	SystemID, DatabaseOID           string
}

// The identity query only touches catalogs every login role may read. The
// executor cannot read wbd_auth.target_marker, so sameness of the physical
// database is proven with the cluster system identifier plus the database
// OID and name, which all three roles can read.
const factsQuery = `SELECT pg_catalog.current_database()::text,session_user::text,
pg_catalog.current_setting('server_version_num')::int,pg_catalog.current_setting('server_encoding'),
pg_catalog.pg_is_in_recovery(),
r.rolsuper OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolbypassrls
  OR EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members WHERE member=r.oid),
(SELECT system_identifier::text FROM pg_catalog.pg_control_system()),
(SELECT oid::text FROM pg_catalog.pg_database WHERE datname=pg_catalog.current_database())
FROM pg_catalog.pg_roles r WHERE r.rolname=session_user`

func readFacts(ctx context.Context, conn *pgx.Conn) (sessionFacts, error) {
	var f sessionFacts
	err := conn.QueryRow(ctx, factsQuery).Scan(&f.Database, &f.SessionUser, &f.Version, &f.Encoding, &f.Recovery, &f.Unsafe, &f.SystemID, &f.DatabaseOID)
	return f, err
}

func connectAs(ctx context.Context, cfg targetConfig, cred credential, role string) (*pgx.Conn, error) {
	cc, err := connConfig(cfg, cred)
	if err != nil {
		return nil, unverified(role + " connection parameters are invalid")
	}
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return nil, unverified(role + " connection failed")
	}
	return conn, nil
}

func checkFacts(role string, f sessionFacts, cfg targetConfig, user string) error {
	switch {
	case f.SessionUser != user:
		return unverified(role + " session role mismatch")
	case f.Database != cfg.Database:
		return unverified(role + " database mismatch")
	case f.Recovery:
		return unverified("target is in recovery")
	case f.Encoding != "UTF8":
		return unverified("target encoding is not UTF8")
	case f.Version < 160000 || f.Version >= 170000:
		return unverified("unsupported server version")
	case f.Unsafe:
		return unverified(role + " role has unsafe attributes")
	}
	return nil
}

// verifyTarget proves that the supplied preparer, executor and viewer
// credentials all reach the same physical database and returns the marker's
// physical_target_id. The id is never derived from anything else.
func verifyTarget(ctx context.Context, cfg targetConfig) (uuid.UUID, error) {
	if !cfg.hasRoles() || !validRolePair(cfg.Executor.User, cfg.Preparer.User) ||
		cfg.Viewer.User == cfg.Executor.User || cfg.Viewer.User == cfg.Preparer.User {
		return uuid.Nil, unverified("role names are invalid")
	}
	prep, err := connectAs(ctx, cfg, cfg.Preparer, "preparer")
	if err != nil {
		return uuid.Nil, err
	}
	defer prep.Close(context.Background())
	pf, err := readFacts(ctx, prep)
	if err != nil {
		return uuid.Nil, unverified("preparer identity query failed: " + sqlState(err))
	}
	if err := checkFacts("preparer", pf, cfg, cfg.Preparer.User); err != nil {
		return uuid.Nil, err
	}
	var marker uuid.UUID
	var markerRole string
	if err := prep.QueryRow(ctx, `SELECT physical_target_id,executor_role::text FROM wbd_auth.target_marker WHERE singleton`).Scan(&marker, &markerRole); err != nil {
		return uuid.Nil, unverified("target marker is unreadable")
	}
	if marker == uuid.Nil || markerRole != cfg.Executor.User ||
		strings.TrimSuffix(markerRole, "_executor")+"_preparer" != cfg.Preparer.User {
		return uuid.Nil, unverified("target marker does not match the supplied roles")
	}

	exec, err := connectAs(ctx, cfg, cfg.Executor, "executor")
	if err != nil {
		return uuid.Nil, err
	}
	defer exec.Close(context.Background())
	ef, err := readFacts(ctx, exec)
	if err != nil {
		return uuid.Nil, unverified("executor identity query failed")
	}
	if err := checkFacts("executor", ef, cfg, cfg.Executor.User); err != nil {
		return uuid.Nil, err
	}
	if ef.SystemID != pf.SystemID || ef.DatabaseOID != pf.DatabaseOID || ef.Database != pf.Database {
		return uuid.Nil, unverified("executor and preparer reach different databases")
	}

	viewer, err := connectAs(ctx, cfg, cfg.Viewer, "viewer")
	if err != nil {
		return uuid.Nil, err
	}
	defer viewer.Close(context.Background())
	vf, err := readFacts(ctx, viewer)
	if err != nil {
		return uuid.Nil, unverified("viewer identity query failed")
	}
	if err := checkFacts("viewer", vf, cfg, cfg.Viewer.User); err != nil {
		return uuid.Nil, err
	}
	if vf.SystemID != pf.SystemID || vf.DatabaseOID != pf.DatabaseOID {
		return uuid.Nil, unverified("viewer and preparer reach different databases")
	}
	var canWrite bool
	if err := viewer.QueryRow(ctx, `SELECT pg_catalog.has_database_privilege(pg_catalog.current_database(),'CREATE')
OR EXISTS (SELECT 1 FROM pg_catalog.pg_namespace n WHERE n.nspname IN ('managed','wbd_auth')
           AND pg_catalog.has_schema_privilege(n.oid,'CREATE'))`).Scan(&canWrite); err != nil || canWrite {
		return uuid.Nil, unverified("viewer role is not read-only")
	}
	return marker, nil
}

// sqlState returns only the SQLSTATE of a server error (never its text).
func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return "client"
}
