package sqlpolicy

import (
	"strings"
	"testing"
)

func TestNewRequiresDistinctManagedSchemas(t *testing.T) {
	for _, schemas := range [][]string{nil, {""}, {"managed", "managed"}, {"public"}, {"information_schema"}, {"wbd_auth"}, {"pg_custom"}, {"_platform_internal"}, {strings.Repeat("x", 64)}, {"bad\x00schema"}, {string([]byte{0xff})}} {
		if _, err := New(schemas...); err == nil {
			t.Errorf("accepted schemas %q", schemas)
		}
	}
	for _, schemas := range [][]string{{"managed"}, {"managed", "another"}, {strings.Repeat("x", 63)}} {
		if _, err := New(schemas...); err != nil {
			t.Errorf("rejected schemas %q: %v", schemas, err)
		}
	}
}

func TestValidateSupportedMigrationKinds(t *testing.T) {
	p := newPolicy(t)
	for _, sql := range []string{
		`CREATE TABLE managed.t (id int PRIMARY KEY, name varchar(50) NOT NULL, amount numeric(10,2) DEFAULT 0, CHECK (amount >= 0))`,
		`ALTER TABLE managed.t ADD COLUMN active boolean DEFAULT true`,
		`ALTER TABLE managed.t ALTER COLUMN name SET NOT NULL`,
		`ALTER TABLE managed.t ALTER COLUMN name DROP NOT NULL`,
		`ALTER TABLE managed.t ALTER COLUMN amount TYPE numeric(12,2)`,
		`ALTER TABLE managed.t DROP COLUMN IF EXISTS name`,
		`ALTER TABLE managed.t ADD CONSTRAINT fk FOREIGN KEY (id) REFERENCES managed.other(id) ON DELETE CASCADE`,
		`ALTER TABLE managed.t DROP CONSTRAINT IF EXISTS fk`,
		`ALTER TABLE managed.t RENAME TO renamed`,
		`ALTER TABLE managed.t RENAME COLUMN name TO label`,
		`CREATE UNIQUE INDEX idx ON managed.t (id DESC NULLS LAST) WHERE id IS NOT NULL`,
		`DROP INDEX IF EXISTS managed.idx`,
		`DROP TABLE IF EXISTS managed.t RESTRICT`,
		`INSERT INTO managed.t (id,name) VALUES (1,'x'),(2,'y') RETURNING id`,
		`UPDATE managed.t SET name='z' WHERE id = 1 OR id IS NULL RETURNING id`,
		`DELETE FROM managed.t WHERE NOT (id > 3) RETURNING id`,
		`CREATE TABLE managed."quoted table" ("quoted column" text); INSERT INTO managed."quoted table" VALUES ('; COMMIT;');`,
	} {
		t.Run(sql, func(t *testing.T) {
			if err := p.Validate(sql); err != nil {
				t.Fatalf("rejected supported SQL: %v", err)
			}
		})
	}
}

func TestValidateRejectsUnsafeOrUnsupportedSQL(t *testing.T) {
	p := newPolicy(t)
	for _, sql := range []string{
		`BEGIN`, `COMMIT`, `ROLLBACK`, `SAVEPOINT x`, `SET ROLE admin`, `SET search_path=managed`,
		`GRANT ALL ON managed.t TO public`, `REVOKE ALL ON managed.t FROM public`, `CREATE ROLE attacker`,
		`CREATE SCHEMA other`, `ALTER TABLE managed.t SET SCHEMA other`, `DROP SCHEMA managed`,
		`CREATE TABLE public.t(id int)`, `CREATE TABLE unqualified(id int)`, `DELETE FROM wbd_auth.execution_receipts`,
		`CREATE TEMP TABLE managed.t(id int)`, `CREATE UNLOGGED TABLE managed.t(id int)`,
		`DROP TABLE managed.t CASCADE`, `ALTER TABLE managed.t DROP COLUMN id CASCADE`,
		`SELECT 1`, `COPY managed.t TO PROGRAM 'command'`, `DO $$ BEGIN NULL; END $$`,
		`CREATE FUNCTION managed.f() RETURNS int LANGUAGE SQL AS 'SELECT 1'`,
		`INSERT INTO managed.t VALUES (pg_sleep(1))`, `INSERT INTO managed.t SELECT id FROM managed.other`,
		`UPDATE managed.t SET id=nextval('managed.seq')`, `DELETE FROM managed.t USING managed.other WHERE true`,
		`CREATE TABLE managed.t(id serial)`, `CREATE TABLE managed.t(id int GENERATED ALWAYS AS IDENTITY)`,
		`CREATE INDEX idx ON managed.t USING hash(id)`, `CREATE INDEX idx ON managed.t ((id+1))`,
		`CREATE TABLE managed.t(id int); COMMIT;`, `CREATE TABLE managed.t(id int); DROP TABLE public.t;`,
		`UPDATE managed.t SET id=(SELECT 1)`, `TRUNCATE managed.t`, `not valid sql`,
	} {
		t.Run(sql, func(t *testing.T) {
			if err := p.Validate(sql); err == nil {
				t.Fatal("accepted unsupported SQL")
			}
		})
	}
}

func TestValidateInputAndBatchBoundaries(t *testing.T) {
	p := newPolicy(t)
	for _, sql := range []string{"", " \n -- comment only", "DELETE FROM managed.t\x00", string([]byte{0xff}), strings.Repeat(" ", maxSQLBytes+1), strings.Repeat("DELETE FROM managed.t;", 257)} {
		if err := p.Validate(sql); err == nil {
			t.Fatalf("accepted invalid input of %d bytes", len(sql))
		}
	}
	if err := p.Validate(strings.Repeat("DELETE FROM managed.t;", 256)); err != nil {
		t.Fatalf("rejected max statements: %v", err)
	}
	base := "DELETE FROM managed.t;"
	if err := p.Validate(base + strings.Repeat(" ", maxSQLBytes-len(base))); err != nil {
		t.Fatalf("rejected max bytes: %v", err)
	}
	var nilPolicy *Policy
	if nilPolicy.Validate(base) == nil || (&Policy{}).Validate(base) == nil {
		t.Fatal("unconfigured policy accepted SQL")
	}
}
