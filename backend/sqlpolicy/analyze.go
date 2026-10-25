package sqlpolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	pgquery "github.com/wasilibs/go-pgquery"
)

// Rel adalah nama relasi yang dikualifikasi skema seperti yang ditulis pengguna.
type Rel struct{ Schema, Name string }

// Jenis statement hasil Analyze.
const (
	KindCreateTable  = "CREATE_TABLE"
	KindAlterTable   = "ALTER_TABLE"
	KindRenameTable  = "RENAME_TABLE"
	KindRenameColumn = "RENAME_COLUMN"
	KindDropTable    = "DROP_TABLE"
	KindDropIndex    = "DROP_INDEX"
	KindCreateIndex  = "CREATE_INDEX"
	KindInsert       = "INSERT"
	KindUpdate       = "UPDATE"
	KindDelete       = "DELETE"
)

// Statement menjelaskan satu statement dalam batch yang sudah lolos Validate.
type Statement struct {
	// Text adalah potongan persis dari SQL asli (tanpa spasi/titik koma di tepi).
	Text string
	Kind string
	// Relation adalah relasi utama (tabel; untuk CREATE INDEX adalah tabelnya).
	Relation Rel
	// Drops berisi seluruh target DROP TABLE/INDEX.
	Drops []Rel
	// NewName adalah nama baru pada RENAME TABLE.
	NewName string
	// Index adalah nama indeks eksplisit pada CREATE INDEX (kosong bila otomatis).
	Index string
	// RefTables adalah tabel yang dirujuk foreign key pada statement ini.
	RefTables []Rel
	// TypeNames adalah nama tipe (terkualifikasi seperti di AST) yang muncul.
	TypeNames [][]string
	// IfNotExists untuk CREATE TABLE/INDEX; MissingOK untuk IF EXISTS.
	IfNotExists, MissingOK bool
}

// Schemas mengembalikan skema yang diizinkan, terurut.
func (p *Policy) Schemas() []string {
	out := make([]string, 0, len(p.schemas))
	for s := range p.schemas {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

type rawStmt struct {
	Stmt     json.RawMessage `json:"stmt"`
	Location int             `json:"stmt_location"`
	Length   int             `json:"stmt_len"`
}

func parseRaw(sql string) ([]rawStmt, error) {
	parsed, err := pgquery.ParseToJSON(sql)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL syntax")
	}
	var tree struct {
		Version int       `json:"version"`
		Stmts   []rawStmt `json:"stmts"`
	}
	d := json.NewDecoder(strings.NewReader(parsed))
	d.DisallowUnknownFields()
	if err := d.Decode(&tree); err != nil || tree.Version != parserVersion {
		return nil, errors.New("unsupported parser AST version")
	}
	return tree.Stmts, nil
}

// Statements mengembalikan potongan teks asli setiap statement, berdasarkan
// offset byte dari parser (stmt_location/stmt_len; panjang 0 berarti sampai
// akhir). Teks tidak dinormalisasi; hanya spasi dan titik koma di tepi yang
// dibuang. Validate dijalankan lebih dulu, dan setiap potongan dipastikan
// berdiri sendiri sebagai tepat satu statement yang juga lolos Validate.
func (p *Policy) Statements(sql string) ([]string, error) {
	infos, err := p.Analyze(sql)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(infos))
	for i, s := range infos {
		out[i] = s.Text
	}
	return out, nil
}

// Analyze memvalidasi batch lalu mengembalikan ringkasan terstruktur tiap
// statement untuk pemeriksaan katalog oleh executor.
func (p *Policy) Analyze(sql string) ([]Statement, error) {
	if err := p.Validate(sql); err != nil {
		return nil, err
	}
	stmts, err := parseRaw(sql)
	if err != nil {
		return nil, err
	}
	out := make([]Statement, 0, len(stmts))
	prevEnd := 0
	for i, st := range stmts {
		start, end := st.Location, len(sql)
		if st.Length > 0 {
			end = st.Location + st.Length
		}
		if start < prevEnd || start < 0 || end > len(sql) || start >= end {
			return nil, fmt.Errorf("statement %d: invalid source offsets", i+1)
		}
		prevEnd = end
		text := strings.TrimRight(strings.TrimSpace(sql[start:end]), "; \t\r\n")
		text = strings.TrimSpace(text)
		if text == "" {
			return nil, fmt.Errorf("statement %d: empty statement text", i+1)
		}
		info, err := describe(st.Stmt)
		if err != nil {
			return nil, fmt.Errorf("statement %d: %w", i+1, err)
		}
		info.Text = text
		out = append(out, info)
	}
	// Setiap potongan harus berdiri sendiri sebagai satu statement.
	for i, s := range out {
		if err := p.Validate(s.Text); err != nil {
			return nil, fmt.Errorf("statement %d: slice is not a valid statement", i+1)
		}
		raw, err := parseRaw(s.Text)
		if err != nil || len(raw) != 1 {
			return nil, fmt.Errorf("statement %d: slice is not a single statement", i+1)
		}
		again, err := describe(raw[0].Stmt)
		if err != nil || again.Kind != s.Kind {
			return nil, fmt.Errorf("statement %d: slice kind mismatch", i+1)
		}
	}
	return out, nil
}

func flag(m object, key string) bool { var b bool; _ = json.Unmarshal(m[key], &b); return b }

func relOf(raw json.RawMessage) (Rel, error) {
	m, err := decode(raw)
	if err != nil {
		return Rel{}, err
	}
	return Rel{Schema: text(m, "schemaname"), Name: text(m, "relname")}, nil
}

func describe(stmt json.RawMessage) (Statement, error) {
	top, err := decode(stmt)
	if err != nil || len(top) != 1 {
		return Statement{}, errors.New("invalid statement node")
	}
	var s Statement
	for kind, payload := range top {
		m, err := decode(payload)
		if err != nil {
			return Statement{}, err
		}
		if m["relation"] != nil {
			if s.Relation, err = relOf(m["relation"]); err != nil {
				return Statement{}, err
			}
		}
		s.MissingOK = flag(m, "missing_ok")
		s.IfNotExists = flag(m, "if_not_exists")
		switch kind {
		case "CreateStmt":
			s.Kind = KindCreateTable
		case "AlterTableStmt":
			s.Kind = KindAlterTable
		case "RenameStmt":
			s.NewName = text(m, "newname")
			if text(m, "renameType") == "OBJECT_COLUMN" {
				s.Kind = KindRenameColumn
			} else {
				s.Kind = KindRenameTable
			}
		case "DropStmt":
			s.Kind = KindDropTable
			if text(m, "removeType") == "OBJECT_INDEX" {
				s.Kind = KindDropIndex
			}
			var objects []json.RawMessage
			if err := json.Unmarshal(m["objects"], &objects); err != nil {
				return Statement{}, err
			}
			for _, o := range objects {
				om, err := decode(o)
				if err != nil || om["List"] == nil {
					return Statement{}, errors.New("invalid DROP name")
				}
				lm, err := decode(om["List"])
				if err != nil {
					return Statement{}, err
				}
				names, err := stringNodes(lm["items"])
				if err != nil || len(names) != 2 {
					return Statement{}, errors.New("invalid DROP name")
				}
				s.Drops = append(s.Drops, Rel{Schema: names[0], Name: names[1]})
			}
		case "IndexStmt":
			s.Kind = KindCreateIndex
			s.Index = text(m, "idxname")
		case "InsertStmt":
			s.Kind = KindInsert
		case "UpdateStmt":
			s.Kind = KindUpdate
		case "DeleteStmt":
			s.Kind = KindDelete
		default:
			return Statement{}, fmt.Errorf("unsupported statement %s", kind)
		}
		if err := collect(payload, &s); err != nil {
			return Statement{}, err
		}
	}
	return s, nil
}

// collect mencari foreign key (pktable) dan nama tipe (typeName) di seluruh subtree.
func collect(raw json.RawMessage, s *Statement) error {
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		for _, e := range arr {
			if err := collect(e, s); err != nil {
				return err
			}
		}
		return nil
	}
	var m object
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil
	}
	for k, v := range m {
		switch k {
		case "pktable":
			r, err := relOf(v)
			if err != nil {
				return err
			}
			s.RefTables = append(s.RefTables, r)
		case "typeName":
			tm, err := decode(v)
			if err != nil {
				return err
			}
			names, err := stringNodes(tm["names"])
			if err != nil {
				return err
			}
			s.TypeNames = append(s.TypeNames, names)
		}
		if err := collect(v, s); err != nil {
			return err
		}
	}
	return nil
}
