package migration

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/sqlpolicy"
)

func testPolicy(t *testing.T) *sqlpolicy.Policy {
	t.Helper()
	p, err := sqlpolicy.New("managed")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func plan(t *testing.T, sqlText string, catalog map[sqlpolicy.Rel]relInfo) (*batchPlan, error) {
	t.Helper()
	p := testPolicy(t)
	infos, err := p.Analyze(sqlText)
	if err != nil {
		t.Fatal(err)
	}
	return planBatch(infos, p.Schemas(), func(r sqlpolicy.Rel) (relInfo, error) { return catalog[r], nil })
}

func isUnsupported(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status == 422 && api.Code == "SQL_UNSUPPORTED"
}

func rel(n string) sqlpolicy.Rel { return sqlpolicy.Rel{Schema: "managed", Name: n} }

func TestPlanBatchTracksCreatedAndRenamed(t *testing.T) {
	cat := map[sqlpolicy.Rel]relInfo{rel("old"): {exists: true, kind: "r"}}
	p, err := plan(t, `CREATE TABLE managed.n (id int); INSERT INTO managed.n (id) VALUES (1);
ALTER TABLE managed.old RENAME COLUMN a TO b; INSERT INTO managed.old (id) VALUES (1);`, cat)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.touched) != 1 || p.touched[rel("old")] != modeDDL {
		t.Fatalf("touched %v", p.touched)
	}
	if _, err := plan(t, `DROP TABLE managed.old; INSERT INTO managed.old (id) VALUES (1)`, cat); !isUnsupported(err) {
		t.Fatalf("dropped existing name must not resolve: %v", err)
	}
	if _, err := plan(t, `CREATE TABLE managed.n (id int); DROP TABLE managed.n; DELETE FROM managed.n`, cat); !isUnsupported(err) {
		t.Fatalf("dropped table must not resolve: %v", err)
	}
}

func TestPlanBatchRejections(t *testing.T) {
	cat := map[sqlpolicy.Rel]relInfo{
		rel("view"):  {exists: true, kind: "v"},
		rel("tbl"):   {exists: true, kind: "r"},
		rel("ix"):    {exists: true, kind: "i", table: rel("tbl")},
		rel("other"): {exists: true, kind: "r"},
	}
	for _, sqlText := range []string{
		`INSERT INTO managed.view (id) VALUES (1)`,
		`INSERT INTO managed.ghost (id) VALUES (1)`,
		`DROP TABLE managed.ghost`,
		`DROP INDEX managed.tbl`,
		`DROP INDEX managed.ghost`,
		`CREATE TABLE managed.c (id int REFERENCES managed.ghost(id))`,
		`CREATE INDEX ON managed.ghost (id)`,
		`ALTER TABLE managed.ghost ADD COLUMN x int`,
	} {
		if _, err := plan(t, sqlText, cat); !isUnsupported(err) {
			t.Errorf("%q: want 422, got %v", sqlText, err)
		}
	}
	p, err := plan(t, `DROP INDEX managed.ix; DROP TABLE IF EXISTS managed.ghost; CREATE TABLE managed.c (id int REFERENCES managed.other(id))`, cat)
	if err != nil {
		t.Fatal(err)
	}
	if p.touched[rel("tbl")] != modeDDL || p.touched[rel("other")] != modeRef {
		t.Fatalf("touched %v", p.touched)
	}
}

func TestPreflightRejectsBeforeCatalog(t *testing.T) {
	pf := NewPreflight(testPolicy(t))
	for _, sqlText := range []string{
		`CREATE TABLE public.t (id int)`, `DELETE FROM wbd_auth.execution_receipts`,
		`ALTER TABLE pg_catalog.pg_class ADD COLUMN x int`, `DROP TABLE managed.t CASCADE`,
		`CREATE TABLE information_schema.t (id int)`, `SELECT 1`,
		`CREATE TABLE managed.t (id int REFERENCES public.p(id))`, `CREATE TABLE managed.t (id myschema.mytype)`,
	} {
		// The tx is never used because rejection happens before any catalog query.
		err := pf(context.Background(), &sql.Tx{}, sqlText)
		if !isUnsupported(err) {
			t.Errorf("%q: want 422, got %v", sqlText, err)
		}
	}
}

// ---- live tests (skipped unless MIGRATION_TEST_TARGET_EXECUTOR_URL is set) ----

func liveTx(t *testing.T) (context.Context, *sql.Tx, *sql.Conn) {
	t.Helper()
	url := os.Getenv("MIGRATION_TEST_TARGET_EXECUTOR_URL")
	if url == "" {
		t.Skip("MIGRATION_TEST_TARGET_EXECUTOR_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(ctx, `SELECT pg_catalog.set_config('search_path','pg_catalog,managed,pg_temp',true),
pg_catalog.set_config('standard_conforming_strings','on',true),pg_catalog.set_config('statement_timeout','20000',true)`); err != nil {
		t.Fatal(err)
	}
	return ctx, tx, conn
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func managedCount(t *testing.T, ctx context.Context, q rowQuerier) int {
	t.Helper()
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='managed'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func relationExists(t *testing.T, ctx context.Context, conn *sql.Conn, name string) bool {
	t.Helper()
	var ok bool
	if err := conn.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, "managed."+name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestLiveMultiStatementInOneTx(t *testing.T) {
	ctx, tx, conn := liveTx(t)
	before := managedCount(t, ctx, tx)
	p := testPolicy(t)
	sqlText := "CREATE TABLE managed.bt_multi (id int PRIMARY KEY, note text);\nINSERT INTO managed.bt_multi (id, note) VALUES (1, 'a;b'), (2, $$c;d$$);\n-- done; ok\nUPDATE managed.bt_multi SET note = 'x; y' WHERE id = 1;"
	if err := NewPreflight(p)(ctx, tx, sqlText); err != nil {
		t.Fatal(err)
	}
	stmts, err := p.Statements(sqlText)
	if err != nil || !slicesOf(sqlText, stmts) || len(stmts) != 3 {
		t.Fatalf("%v %q", err, stmts)
	}
	// The whole text in one Exec is what used to fail under the extended protocol.
	if _, err := tx.ExecContext(ctx, "SAVEPOINT s"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, sqlText); err == nil {
		t.Log("note: multi-statement text unexpectedly accepted by driver")
	}
	if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
		t.Fatal(err)
	}
	if err := execStatements(ctx, tx, stmts); err != nil {
		t.Fatal(err)
	}
	var n int
	var notes string
	if err := tx.QueryRowContext(ctx, `SELECT count(*), string_agg(note, '|' ORDER BY id) FROM managed.bt_multi`).Scan(&n, &notes); err != nil || n != 2 || notes != "x; y|c;d" {
		t.Fatalf("rows %d %q %v", n, notes, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if relationExists(t, ctx, conn, "bt_multi") || managedCount(t, ctx, conn) != before {
		t.Fatal("table survived rollback")
	}
}

func TestLiveSecondStatementFailureRollsBackAll(t *testing.T) {
	ctx, tx, conn := liveTx(t)
	before := managedCount(t, ctx, tx)
	sqlText := `CREATE TABLE managed.bt_fail (id int); INSERT INTO managed.bt_fail (nope) VALUES (1)`
	p := testPolicy(t)
	stmts, err := p.Statements(sqlText)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewPreflight(p)(ctx, tx, sqlText); err != nil {
		t.Fatal(err)
	}
	if err := execStatements(ctx, tx, stmts); err == nil {
		t.Fatal("expected failure")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if relationExists(t, ctx, conn, "bt_fail") {
		t.Fatal("first statement survived")
	}
	if got := managedCount(t, ctx, conn); got != before {
		t.Fatalf("managed relations %d -> %d", before, got)
	}
}

func TestLiveRejectsUnsafeExistingTables(t *testing.T) {
	ctx, tx, _ := liveTx(t)
	pf := NewPreflight(testPolicy(t))
	setup := []string{
		`CREATE TABLE managed.bt_clean (id int PRIMARY KEY, created timestamptz DEFAULT now(), n int CHECK (n > 0))`,
		`CREATE UNIQUE INDEX bt_clean_n ON managed.bt_clean (n)`,
		`CREATE TABLE managed.bt_child (id int, p int REFERENCES managed.bt_clean(id))`,
		`CREATE TABLE managed.bt_trig (id int)`,
		`CREATE FUNCTION managed.bt_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`,
		`CREATE TRIGGER bt_tr BEFORE INSERT ON managed.bt_trig FOR EACH ROW EXECUTE FUNCTION managed.bt_fn()`,
		`CREATE TABLE managed.bt_rls (id int)`,
		`ALTER TABLE managed.bt_rls ENABLE ROW LEVEL SECURITY`,
		`CREATE TABLE managed.bt_rule (id int)`,
		`CREATE RULE bt_rule_r AS ON INSERT TO managed.bt_rule DO ALSO NOTHING`,
		`CREATE FUNCTION managed.bt_id() RETURNS int LANGUAGE sql AS 'SELECT 1'`,
		`CREATE TABLE managed.bt_dflt (id int DEFAULT managed.bt_id())`,
		`CREATE TABLE managed.bt_view_base (id int)`,
		`CREATE VIEW managed.bt_view AS SELECT id FROM managed.bt_view_base`,
		`CREATE TABLE managed.bt_fk_parent (id int PRIMARY KEY)`,
		`CREATE TABLE managed.bt_fk_child (id int, p int REFERENCES managed.bt_fk_parent(id))`,
		`CREATE TRIGGER bt_tr2 BEFORE INSERT ON managed.bt_fk_child FOR EACH ROW EXECUTE FUNCTION managed.bt_fn()`,
	}
	for i, s := range setup {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			t.Fatalf("setup %d (%s): %v", i, s, err)
		}
	}
	for _, ok := range []string{
		`INSERT INTO managed.bt_clean (id, n) VALUES (1, 1)`,
		`ALTER TABLE managed.bt_clean ADD COLUMN extra text`,
		`INSERT INTO managed.bt_child (id, p) VALUES (1, 1); DROP INDEX managed.bt_clean_n`,
		`CREATE TABLE managed.bt_new (id int REFERENCES managed.bt_clean(id)); INSERT INTO managed.bt_new (id) VALUES (1)`,
	} {
		if err := pf(ctx, tx, ok); err != nil {
			t.Errorf("%q should pass: %v", ok, err)
		}
	}
	for name, sqlText := range map[string]string{
		"trigger":           `INSERT INTO managed.bt_trig (id) VALUES (1)`,
		"rls":               `DELETE FROM managed.bt_rls`,
		"rule":              `UPDATE managed.bt_rule SET id = 2`,
		"foreign default":   `INSERT INTO managed.bt_dflt (id) VALUES (1)`,
		"view":              `INSERT INTO managed.bt_view (id) VALUES (1)`,
		"fk neighbour trig": `DELETE FROM managed.bt_fk_parent`,
		"public ddl":        `CREATE TABLE public.bt_x (id int)`,
		"wbd_auth dml":      `DELETE FROM wbd_auth.execution_receipts`,
		"missing":           `INSERT INTO managed.bt_nowhere (id) VALUES (1)`,
		"fk to neighbour":   `CREATE TABLE managed.bt_z (id int REFERENCES managed.bt_fk_parent(id))`,
	} {
		err := pf(ctx, tx, sqlText)
		if !isUnsupported(err) {
			t.Errorf("%s: want 422, got %v", name, err)
		} else if strings.Contains(err.Error(), "bt_fn") {
			t.Errorf("%s: message leaks internal names: %v", name, err)
		}
	}
}
