package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/sqlpolicy"
)

// NewPreflight membuat pemeriksaan keselamatan target yang fail-closed. Ia
// berjalan pada transaksi executor yang sama (terpasang pada koneksi yang
// di-pin) dengan transaksi eksekusi, sehingga kunci tabel yang diambil di sini
// bertahan sampai commit/rollback.
//
// Subset yang didukung (setelah lolos sqlpolicy.Validate, yang menolak CASCADE,
// fungsi, subquery, tipe non-builtin, dan skema selain skema terkelola):
//   - CREATE TABLE, ALTER TABLE (add/drop column, alter type, default, not null,
//     add/drop constraint), RENAME TABLE/COLUMN, DROP TABLE/INDEX (RESTRICT),
//     CREATE INDEX (btree), INSERT ... VALUES, UPDATE, DELETE.
//   - Relasi yang sudah ada harus berada di skema terkelola, bertipe tabel biasa
//     (relkind 'r'; indeks 'i' hanya untuk DROP INDEX), dimiliki current_user.
//   - Relasi yang dibuat atau di-rename di dalam batch dilacak sehingga
//     statement berikutnya boleh memakainya tanpa pemeriksaan katalog.
//   - Tabel yang ada dikunci pada transaksi yang sama (ACCESS EXCLUSIVE untuk
//     DDL, SHARE ROW EXCLUSIVE untuk target foreign key, ROW EXCLUSIVE untuk DML
//     dan tabel terhubung foreign key) dengan lock_timeout, lalu katalog
//     diperiksa ulang setelah kunci didapat.
//
// Ditolak dengan sengaja (422 SQL_UNSUPPORTED): relasi yang tidak ada dan tidak
// dibuat dalam batch (kecuali IF EXISTS / IF NOT EXISTS), view, tabel
// partisi/turunan, tabel bukan milik current_user, trigger non-internal, rule,
// RLS atau policy, constraint exclusion/trigger, default, generated column,
// CHECK, atau indeks yang memakai fungsi/operator/tipe/collation di luar
// pg_catalog, kolom bertipe non-pg_catalog, foreign key dari/ke tabel di luar
// skema terkelola (diperiksa transitif), DROP INDEX untuk indeks otomatis
// yang dibuat dalam batch yang sama, serta apa pun di wbd_auth, public, pg_*
// atau information_schema. Lock timeout menghasilkan 409 DATABASE_BUSY; kegagalan
// kueri katalog dikembalikan sebagai error biasa (dipetakan executor ke 502).
//
// Batasan: pemeriksaan hanya berlaku untuk keadaan katalog saat preflight
// ditambah kunci tabel; relasi yang belum ada saat preflight tidak dikunci.
func NewPreflight(policy *sqlpolicy.Policy) func(ctx context.Context, tx *sql.Tx, sqlText string) error {
	return func(ctx context.Context, tx *sql.Tx, sqlText string) error {
		if policy == nil || tx == nil {
			return errors.New("preflight is not configured")
		}
		infos, err := policy.Analyze(sqlText)
		if err != nil {
			return unsupported("SQL is outside the supported subset")
		}
		schemas := policy.Schemas()
		p, err := planBatch(infos, schemas, func(r sqlpolicy.Rel) (relInfo, error) { return lookupRelation(ctx, tx, r) })
		if err != nil {
			return err
		}
		return p.lockAndVerify(ctx, tx, schemas)
	}
}

func unsupported(message string) error { return fault(422, "SQL_UNSUPPORTED", message) }

const (
	modeDML = iota + 1 // ROW EXCLUSIVE
	modeRef            // SHARE ROW EXCLUSIVE
	modeDDL            // ACCESS EXCLUSIVE
)

var lockModes = map[int]string{modeDML: "ROW EXCLUSIVE", modeRef: "SHARE ROW EXCLUSIVE", modeDDL: "ACCESS EXCLUSIVE"}

type relInfo struct {
	exists bool
	kind   string
	table  sqlpolicy.Rel // tabel pemilik, hanya untuk indeks
}

type batchPlan struct {
	touched map[sqlpolicy.Rel]int
}

type relState int

const (
	stExisting relState = iota + 1
	stCreated
	stDropped
)

func shown(r sqlpolicy.Rel) string { return fmt.Sprintf("%q.%q", r.Schema, r.Name) }

// planBatch menelusuri batch secara berurutan dengan pelacakan relasi yang
// dibuat/di-rename/di-drop, dan mengumpulkan relasi yang sudah ada.
func planBatch(infos []sqlpolicy.Statement, schemas []string, lookup func(sqlpolicy.Rel) (relInfo, error)) (*batchPlan, error) {
	allowed := map[string]bool{}
	for _, s := range schemas {
		allowed[s] = true
	}
	checkRel := func(r sqlpolicy.Rel) error {
		if !allowed[r.Schema] || r.Schema == "wbd_auth" || r.Schema == "public" || r.Schema == "information_schema" || strings.HasPrefix(r.Schema, "pg_") || r.Name == "" {
			return unsupported("Object " + shown(r) + " is outside the managed schema")
		}
		return nil
	}
	plan := &batchPlan{touched: map[sqlpolicy.Rel]int{}}
	state := map[sqlpolicy.Rel]relState{}
	createdIdx := map[sqlpolicy.Rel]bool{}
	droppedIdx := map[sqlpolicy.Rel]bool{}
	touch := func(r sqlpolicy.Rel, mode int) {
		if plan.touched[r] < mode {
			plan.touched[r] = mode
		}
	}
	// resolveTable mengembalikan true bila tabel ada (di katalog atau dibuat dalam batch).
	resolveTable := func(r sqlpolicy.Rel, mode int) (bool, error) {
		switch state[r] {
		case stCreated:
			return true, nil
		case stDropped:
			return false, nil
		case stExisting:
			touch(r, mode)
			return true, nil
		}
		info, err := lookup(r)
		if err != nil {
			return false, err
		}
		if !info.exists {
			return false, nil
		}
		if info.kind != "r" {
			return false, unsupported("Object " + shown(r) + " is not an ordinary table")
		}
		state[r] = stExisting
		touch(r, mode)
		return true, nil
	}
	missing := func(r sqlpolicy.Rel) error {
		return unsupported("Table " + shown(r) + " does not exist or cannot be verified")
	}
	for _, st := range infos {
		for _, names := range st.TypeNames {
			if len(names) > 2 || len(names) == 2 && names[0] != "pg_catalog" {
				return nil, unsupported("Only built-in column types are supported")
			}
		}
		all := append([]sqlpolicy.Rel{}, st.RefTables...)
		all = append(all, st.Drops...)
		if st.Relation.Name != "" {
			all = append(all, st.Relation)
		}
		for _, r := range all {
			if err := checkRel(r); err != nil {
				return nil, err
			}
		}
		refs := func() error {
			for _, ref := range st.RefTables {
				ok, err := resolveTable(ref, modeRef)
				if err != nil {
					return err
				}
				if !ok {
					return missing(ref)
				}
			}
			return nil
		}
		switch st.Kind {
		case sqlpolicy.KindCreateTable:
			ok, err := resolveTable(st.Relation, modeDDL)
			if err != nil {
				return nil, err
			}
			if !ok {
				state[st.Relation] = stCreated
			}
			if err := refs(); err != nil {
				return nil, err
			}
		case sqlpolicy.KindAlterTable, sqlpolicy.KindRenameColumn:
			ok, err := resolveTable(st.Relation, modeDDL)
			if err != nil {
				return nil, err
			}
			if !ok {
				if st.MissingOK {
					continue
				}
				return nil, missing(st.Relation)
			}
			if err := refs(); err != nil {
				return nil, err
			}
		case sqlpolicy.KindRenameTable:
			ok, err := resolveTable(st.Relation, modeDDL)
			if err != nil {
				return nil, err
			}
			if !ok {
				if st.MissingOK {
					continue
				}
				return nil, missing(st.Relation)
			}
			newRel := sqlpolicy.Rel{Schema: st.Relation.Schema, Name: st.NewName}
			if err := checkRel(newRel); err != nil {
				return nil, err
			}
			state[st.Relation] = stDropped
			state[newRel] = stCreated
		case sqlpolicy.KindDropTable:
			for _, r := range st.Drops {
				ok, err := resolveTable(r, modeDDL)
				if err != nil {
					return nil, err
				}
				if !ok {
					if st.MissingOK {
						continue
					}
					return nil, missing(r)
				}
				state[r] = stDropped
			}
		case sqlpolicy.KindDropIndex:
			for _, r := range st.Drops {
				if createdIdx[r] {
					delete(createdIdx, r)
					droppedIdx[r] = true
					continue
				}
				if droppedIdx[r] {
					if st.MissingOK {
						continue
					}
					return nil, unsupported("Index " + shown(r) + " does not exist or cannot be verified")
				}
				info, err := lookup(r)
				if err != nil {
					return nil, err
				}
				if !info.exists {
					if st.MissingOK {
						continue
					}
					return nil, unsupported("Index " + shown(r) + " does not exist or cannot be verified")
				}
				if info.kind != "i" {
					return nil, unsupported("Object " + shown(r) + " is not an index")
				}
				if err := checkRel(info.table); err != nil {
					return nil, err
				}
				ok, err := resolveTable(info.table, modeDDL)
				if err != nil {
					return nil, err
				}
				if !ok {
					return nil, unsupported("Index " + shown(r) + " does not exist or cannot be verified")
				}
				droppedIdx[r] = true
			}
		case sqlpolicy.KindCreateIndex:
			ok, err := resolveTable(st.Relation, modeDDL)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, missing(st.Relation)
			}
			if st.Index != "" {
				createdIdx[sqlpolicy.Rel{Schema: st.Relation.Schema, Name: st.Index}] = true
			}
		case sqlpolicy.KindInsert, sqlpolicy.KindUpdate, sqlpolicy.KindDelete:
			ok, err := resolveTable(st.Relation, modeDML)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, missing(st.Relation)
			}
		default:
			return nil, unsupported("SQL is outside the supported subset")
		}
	}
	return plan, nil
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func lookupRelation(ctx context.Context, q querier, r sqlpolicy.Rel) (relInfo, error) {
	var kind, tschema, tname sql.NullString
	err := q.QueryRowContext(ctx, `SELECT c.relkind::text, tn.nspname::text, t.relname::text
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_index i ON i.indexrelid = c.oid
LEFT JOIN pg_catalog.pg_class t ON t.oid = i.indrelid
LEFT JOIN pg_catalog.pg_namespace tn ON tn.oid = t.relnamespace
WHERE n.nspname = $1 AND c.relname = $2`, r.Schema, r.Name).Scan(&kind, &tschema, &tname)
	if errors.Is(err, sql.ErrNoRows) {
		return relInfo{}, nil
	}
	if err != nil {
		return relInfo{}, err
	}
	return relInfo{exists: true, kind: kind.String, table: sqlpolicy.Rel{Schema: tschema.String, Name: tname.String}}, nil
}

const closureCTE = `WITH RECURSIVE seed AS (
  SELECT c.oid FROM pg_catalog.pg_class c
  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
  JOIN ROWS FROM (pg_catalog.unnest($1::text[]), pg_catalog.unnest($2::text[])) AS t(s, r) ON n.nspname = t.s AND c.relname = t.r
), closure(oid) AS (
  SELECT oid FROM seed
  UNION
  SELECT CASE WHEN k.conrelid = cl.oid THEN k.confrelid ELSE k.conrelid END
  FROM closure cl JOIN pg_catalog.pg_constraint k ON k.contype = 'f' AND (k.conrelid = cl.oid OR k.confrelid = cl.oid)
)
`

const discoverSQL = closureCTE + `SELECT n.nspname::text, c.relname::text FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE c.oid IN (SELECT oid FROM closure)`

// verifySQL mengembalikan satu baris per relasi dalam closure foreign key.
const verifySQL = closureCTE + `SELECT n.nspname::text, c.relname::text, c.relkind::text,
 pg_catalog.pg_get_userbyid(c.relowner) = current_user AS owned,
 n.nspname = ANY($3::text[]) AS managed,
 EXISTS (SELECT 1 FROM pg_catalog.pg_trigger t WHERE t.tgrelid = c.oid AND NOT t.tgisinternal) AS has_trigger,
 EXISTS (SELECT 1 FROM pg_catalog.pg_rewrite w WHERE w.ev_class = c.oid) AS has_rule,
 (c.relrowsecurity OR c.relforcerowsecurity OR EXISTS (SELECT 1 FROM pg_catalog.pg_policy p WHERE p.polrelid = c.oid)) AS has_rls,
 (c.relispartition OR EXISTS (SELECT 1 FROM pg_catalog.pg_inherits h WHERE h.inhrelid = c.oid OR h.inhparent = c.oid)) AS has_inherit,
 EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k WHERE k.conrelid = c.oid AND k.contype IN ('x', 't')) AS has_odd_constraint,
 EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_type ty ON ty.oid = a.atttypid
         WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped AND ty.typnamespace <> 'pg_catalog'::regnamespace) AS has_custom_type,
 EXISTS (SELECT 1 FROM pg_catalog.pg_depend d
   WHERE (   (d.classid = 'pg_catalog.pg_attrdef'::regclass AND d.objid IN (SELECT ad.oid FROM pg_catalog.pg_attrdef ad WHERE ad.adrelid = c.oid))
          OR (d.classid = 'pg_catalog.pg_constraint'::regclass AND d.objid IN (SELECT k.oid FROM pg_catalog.pg_constraint k WHERE k.conrelid = c.oid AND k.contype = 'c'))
          OR (d.classid = 'pg_catalog.pg_class'::regclass AND d.objid IN (SELECT ix.indexrelid FROM pg_catalog.pg_index ix WHERE ix.indrelid = c.oid)))
     AND NOT (d.refclassid = 'pg_catalog.pg_class'::regclass AND (d.refobjid = c.oid OR d.refobjid IN (SELECT ix.indexrelid FROM pg_catalog.pg_index ix WHERE ix.indrelid = c.oid)))
     AND NOT (d.refclassid = 'pg_catalog.pg_constraint'::regclass AND d.refobjid IN (SELECT k.oid FROM pg_catalog.pg_constraint k WHERE k.conrelid = c.oid))
     AND (CASE d.refclassid
            WHEN 'pg_catalog.pg_proc'::regclass THEN (SELECT x.pronamespace FROM pg_catalog.pg_proc x WHERE x.oid = d.refobjid)
            WHEN 'pg_catalog.pg_operator'::regclass THEN (SELECT x.oprnamespace FROM pg_catalog.pg_operator x WHERE x.oid = d.refobjid)
            WHEN 'pg_catalog.pg_type'::regclass THEN (SELECT x.typnamespace FROM pg_catalog.pg_type x WHERE x.oid = d.refobjid)
            WHEN 'pg_catalog.pg_collation'::regclass THEN (SELECT x.collnamespace FROM pg_catalog.pg_collation x WHERE x.oid = d.refobjid)
            WHEN 'pg_catalog.pg_opclass'::regclass THEN (SELECT x.opcnamespace FROM pg_catalog.pg_opclass x WHERE x.oid = d.refobjid)
            WHEN 'pg_catalog.pg_opfamily'::regclass THEN (SELECT x.opfnamespace FROM pg_catalog.pg_opfamily x WHERE x.oid = d.refobjid)
            WHEN 'pg_catalog.pg_class'::regclass THEN (SELECT x.relnamespace FROM pg_catalog.pg_class x WHERE x.oid = d.refobjid)
          END) IS DISTINCT FROM 'pg_catalog'::regnamespace) AS has_foreign_dependency
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.oid IN (SELECT oid FROM closure)`

func (p *batchPlan) lockAndVerify(ctx context.Context, tx querier, schemas []string) error {
	if len(p.touched) == 0 {
		return nil
	}
	seedS, seedN := make([]string, 0, len(p.touched)), make([]string, 0, len(p.touched))
	for r := range p.touched {
		seedS, seedN = append(seedS, r.Schema), append(seedN, r.Name)
	}
	allowed := map[string]bool{}
	for _, s := range schemas {
		allowed[s] = true
	}
	// Temukan closure foreign key sebelum mengunci.
	closure, err := readClosure(ctx, tx, seedS, seedN)
	if err != nil {
		return err
	}
	modes := map[sqlpolicy.Rel]int{}
	for r := range closure {
		if !allowed[r.Schema] {
			return unsupported("A foreign key links a touched table to a table outside the managed schema")
		}
		modes[r] = modeDML
	}
	for r, m := range p.touched {
		if _, ok := modes[r]; !ok {
			return fault(409, "DATABASE_BUSY", "Target changed during safety check")
		}
		modes[r] = m
	}
	order := make([]sqlpolicy.Rel, 0, len(modes))
	for r := range modes {
		order = append(order, r)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].Schema != order[j].Schema {
			return order[i].Schema < order[j].Schema
		}
		return order[i].Name < order[j].Name
	})
	timeout := 3 * time.Second
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 2*timeout {
		timeout = max(time.Until(deadline)/2, time.Millisecond)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_catalog.set_config('lock_timeout', $1, true)`, fmt.Sprintf("%dms", max(int64(1), timeout.Milliseconds()))); err != nil {
		return err
	}
	for _, r := range order {
		_, err := tx.ExecContext(ctx, "LOCK TABLE "+quoteIdent(r.Schema)+"."+quoteIdent(r.Name)+" IN "+lockModes[modes[r]]+" MODE")
		if err != nil {
			switch sqlState(err) {
			case "55P03", "40P01", "42P01":
				return fault(409, "DATABASE_BUSY", "Target tables are busy")
			case "42501":
				return unsupported("Table " + shown(r) + " is not owned by the executor")
			}
			return err
		}
	}
	// Periksa ulang setelah kunci didapat; closure harus tidak berubah.
	rows, err := tx.QueryContext(ctx, verifySQL, seedS, seedN, schemas)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var schema, name, kind string
		var owned, managed, trig, rule, rls, inh, odd, ctype, dep bool
		if err := rows.Scan(&schema, &name, &kind, &owned, &managed, &trig, &rule, &rls, &inh, &odd, &ctype, &dep); err != nil {
			return err
		}
		r := sqlpolicy.Rel{Schema: schema, Name: name}
		if _, ok := modes[r]; !ok {
			return fault(409, "DATABASE_BUSY", "Target changed during safety check")
		}
		seen++
		label := "Table " + shown(r)
		if _, direct := p.touched[r]; !direct {
			label = "A table linked by foreign key"
		}
		switch {
		case !managed:
			return unsupported(label + " is outside the managed schema")
		case kind != "r":
			return unsupported(label + " is not an ordinary table")
		case !owned:
			return unsupported(label + " is not owned by the executor")
		case trig:
			return unsupported(label + " has triggers, which are not supported")
		case rule:
			return unsupported(label + " has rules, which are not supported")
		case rls:
			return unsupported(label + " uses row level security, which is not supported")
		case inh:
			return unsupported(label + " uses inheritance or partitioning, which is not supported")
		case odd:
			return unsupported(label + " has exclusion or constraint triggers, which are not supported")
		case ctype:
			return unsupported(label + " has columns with non built-in types, which are not supported")
		case dep:
			return unsupported(label + " has defaults, constraints or indexes that depend on non built-in objects")
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if seen != len(modes) {
		return fault(409, "DATABASE_BUSY", "Target changed during safety check")
	}
	return nil
}

func readClosure(ctx context.Context, q querier, schemas, names []string) (map[sqlpolicy.Rel]bool, error) {
	rows, err := q.QueryContext(ctx, discoverSQL, schemas, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[sqlpolicy.Rel]bool{}
	for rows.Next() {
		var r sqlpolicy.Rel
		if err := rows.Scan(&r.Schema, &r.Name); err != nil {
			return nil, err
		}
		out[r] = true
	}
	return out, rows.Err()
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func sqlState(err error) string {
	var s interface{ SQLState() string }
	if errors.As(err, &s) {
		return s.SQLState()
	}
	return ""
}
