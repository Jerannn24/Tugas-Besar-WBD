package sqlpolicy

import (
	"reflect"
	"strings"
	"testing"
)

func newPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := New("managed")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStatementsSplitting(t *testing.T) {
	p := newPolicy(t)
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{"single", `CREATE TABLE managed.t (id int)`, []string{`CREATE TABLE managed.t (id int)`}},
		{"single trailing semicolon and space", "CREATE TABLE managed.t (id int);  \n", []string{`CREATE TABLE managed.t (id int)`}},
		{"two", `CREATE TABLE managed.t (id int); INSERT INTO managed.t (id) VALUES (1);`,
			[]string{`CREATE TABLE managed.t (id int)`, `INSERT INTO managed.t (id) VALUES (1)`}},
		{"semicolon in literal", `INSERT INTO managed.t (a) VALUES ('x; DROP TABLE managed.t; --'); DELETE FROM managed.t`,
			[]string{`INSERT INTO managed.t (a) VALUES ('x; DROP TABLE managed.t; --')`, `DELETE FROM managed.t`}},
		{"escaped quote in literal", `INSERT INTO managed.t (a) VALUES ('it''s;'); DELETE FROM managed.t`,
			[]string{`INSERT INTO managed.t (a) VALUES ('it''s;')`, `DELETE FROM managed.t`}},
		{"quoted identifier with semicolon", `CREATE TABLE managed."a;b" ("c;d" int); DELETE FROM managed."a;b"`,
			[]string{`CREATE TABLE managed."a;b" ("c;d" int)`, `DELETE FROM managed."a;b"`}},
		{"dollar quote literal", `INSERT INTO managed.t (a) VALUES ($$a;b$$); INSERT INTO managed.t (a) VALUES ($q$;$q$)`,
			[]string{`INSERT INTO managed.t (a) VALUES ($$a;b$$)`, `INSERT INTO managed.t (a) VALUES ($q$;$q$)`}},
		{"line comment", "-- lead; comment\nDELETE FROM managed.t; -- mid; comment\nDELETE FROM managed.u",
			[]string{"-- lead; comment\nDELETE FROM managed.t", "-- mid; comment\nDELETE FROM managed.u"}},
		{"block comment", "DELETE FROM managed.t /* a;b */; /* c;d */ DELETE FROM managed.u",
			[]string{"DELETE FROM managed.t /* a;b */", "/* c;d */ DELETE FROM managed.u"}},
		{"multiline", "CREATE TABLE managed.t (\n  id int,\n  n text\n);\n\nINSERT INTO managed.t (id, n)\nVALUES (1, 'a'),\n       (2, 'b');\n",
			[]string{"CREATE TABLE managed.t (\n  id int,\n  n text\n)", "INSERT INTO managed.t (id, n)\nVALUES (1, 'a'),\n       (2, 'b')"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := p.Statements(c.sql)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %q want %q", got, c.want)
			}
			for _, s := range got {
				if !strings.Contains(c.sql, s) {
					t.Fatalf("slice %q is not a substring of the original", s)
				}
			}
		})
	}
}

func TestStatementsRejectsWhatValidateRejects(t *testing.T) {
	p := newPolicy(t)
	for _, sql := range []string{
		``, `SELECT 1`, `CREATE TABLE public.t (id int)`, `DROP TABLE managed.t CASCADE`,
		`DELETE FROM wbd_auth.execution_receipts`, `CREATE TABLE managed.t (id int); DROP SCHEMA managed`,
		`CREATE TABLE managed.t (id int); SELECT pg_sleep(1)`, `CREATE TABLE managed.t (id serial)`,
		`CREATE TABLE managed.t (id int DEFAULT nextval('x'))`,
	} {
		if got, err := p.Statements(sql); err == nil {
			t.Errorf("%q accepted: %q", sql, got)
		}
	}
}

func TestAnalyzeDescribes(t *testing.T) {
	p := newPolicy(t)
	infos, err := p.Analyze(`CREATE TABLE managed.c (id int PRIMARY KEY, p int REFERENCES managed.parent(id));
ALTER TABLE managed.c ADD COLUMN q int, ADD CONSTRAINT fk FOREIGN KEY (q) REFERENCES managed.other(id);
CREATE INDEX ix ON managed.c (q);
ALTER TABLE managed.c RENAME COLUMN q TO d;
DROP TABLE IF EXISTS managed.c, managed.e;
DROP INDEX managed.ix;
ALTER TABLE managed.c RENAME COLUMN q TO r;
UPDATE managed.c SET q = 1;`)
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, i := range infos {
		kinds = append(kinds, i.Kind)
	}
	want := []string{KindCreateTable, KindAlterTable, KindCreateIndex, KindRenameColumn, KindDropTable, KindDropIndex, KindRenameColumn, KindUpdate}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("kinds %v", kinds)
	}
	if !reflect.DeepEqual(infos[0].RefTables, []Rel{{"managed", "parent"}}) || !reflect.DeepEqual(infos[1].RefTables, []Rel{{"managed", "other"}}) {
		t.Fatalf("refs %v %v", infos[0].RefTables, infos[1].RefTables)
	}
	if infos[3].NewName != "d" || !infos[4].MissingOK || len(infos[4].Drops) != 2 || infos[2].Index != "ix" || infos[2].Relation != (Rel{"managed", "c"}) {
		t.Fatalf("unexpected %+v", infos)
	}
	if len(infos[0].TypeNames) == 0 {
		t.Fatal("type names not collected")
	}
}

// PG16-valid statements through the PG17 parser: kinds and offsets match.
func TestPG16StatementsOnPG17Parser(t *testing.T) {
	p := newPolicy(t)
	sql := `CREATE TABLE IF NOT EXISTS managed.a (id bigint NOT NULL, t timestamptz DEFAULT 'now', n numeric(10,2), CHECK (n > 0));
CREATE UNIQUE INDEX IF NOT EXISTS ux ON managed.a (id DESC NULLS LAST) WHERE n IS NOT NULL;
ALTER TABLE IF EXISTS managed.a ALTER COLUMN n TYPE numeric(12,2), ALTER COLUMN id SET NOT NULL, DROP COLUMN IF EXISTS t;
INSERT INTO managed.a (id, n) VALUES (1, 2.5), (2, -1) RETURNING id;
UPDATE managed.a SET n = n + 1 WHERE id = 1 AND NOT (n IS NULL);
DELETE FROM managed.a WHERE id <> 2`
	infos, err := p.Analyze(sql)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{KindCreateTable, KindCreateIndex, KindAlterTable, KindInsert, KindUpdate, KindDelete}
	for i, k := range want {
		if infos[i].Kind != k {
			t.Fatalf("stmt %d kind %s want %s", i, infos[i].Kind, k)
		}
	}
	if !infos[0].IfNotExists || !infos[2].MissingOK {
		t.Fatal("flags lost")
	}
}
