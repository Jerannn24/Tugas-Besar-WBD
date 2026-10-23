// Package sqlpolicy membatasi SQL migrasi ke sintaks yang didukung.
// Pemeriksaan ini tidak menjamin keamanan eksekusi PostgreSQL.
package sqlpolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	pgquery "github.com/wasilibs/go-pgquery"
)

const parserVersion = 170007
const maxSQLBytes = 262144

type object map[string]json.RawMessage

// Policy membatasi akses ke skema milik aplikasi yang diizinkan.
// Dependensi objek tetap harus diperiksa oleh executor.
type Policy struct{ schemas map[string]bool }

func New(schemas ...string) (*Policy, error) {
	if len(schemas) == 0 {
		return nil, errors.New("at least one schema is required")
	}
	p := &Policy{schemas: make(map[string]bool)}
	for _, schema := range schemas {
		if !identifier(schema) || schema == "information_schema" || schema == "public" || schema == "wbd_auth" || strings.HasPrefix(schema, "pg_") || strings.HasPrefix(schema, "_platform") || p.schemas[schema] {
			return nil, errors.New("invalid or duplicate managed schema")
		}
		p.schemas[schema] = true
	}
	return p, nil
}

// Validate memeriksa seluruh batch tanpa mengubah teks SQL.
func (p *Policy) Validate(sql string) error {
	if p == nil || len(p.schemas) == 0 {
		return errors.New("policy is not configured")
	}
	if len(sql) == 0 || len(sql) > maxSQLBytes || !utf8.ValidString(sql) || strings.ContainsRune(sql, 0) {
		return errors.New("SQL must contain 1..262144 valid UTF-8 bytes without NUL")
	}
	parsed, err := pgquery.ParseToJSON(sql)
	if err != nil {
		return errors.New("invalid PostgreSQL syntax")
	}
	var tree struct {
		Version int `json:"version"`
		Stmts   []struct {
			Stmt     json.RawMessage `json:"stmt"`
			Location int             `json:"stmt_location"`
			Length   int             `json:"stmt_len"`
		} `json:"stmts"`
	}
	d := json.NewDecoder(strings.NewReader(parsed))
	d.DisallowUnknownFields()
	if err := d.Decode(&tree); err != nil || tree.Version != parserVersion {
		return errors.New("unsupported parser AST version")
	}
	if len(tree.Stmts) == 0 || len(tree.Stmts) > 256 {
		return errors.New("SQL must contain 1..256 statements")
	}
	v := validator{policy: p}
	for i, statement := range tree.Stmts {
		m, err := decode(statement.Stmt)
		if err != nil {
			return err
		}
		for kind := range m {
			switch kind {
			case "CreateStmt", "AlterTableStmt", "RenameStmt", "DropStmt", "IndexStmt", "InsertStmt", "UpdateStmt", "DeleteStmt":
			default:
				return fmt.Errorf("statement %d: unsupported statement %s", i+1, kind)
			}
		}
		if err := v.node(statement.Stmt); err != nil {
			return fmt.Errorf("statement %d: %w", i+1, err)
		}
	}
	return nil
}

type validator struct {
	policy       *Policy
	depth, count int
}

func (v *validator) node(raw json.RawMessage) error {
	m, err := decode(raw)
	if err != nil {
		return err
	}
	if len(m) != 1 {
		return errors.New("invalid AST node")
	}
	for kind, payload := range m {
		return v.message(kind, payload)
	}
	return errors.New("empty AST node")
}

func (v *validator) message(kind string, raw json.RawMessage) error {
	v.depth++
	v.count++
	defer func() { v.depth-- }()
	if v.depth > 64 || v.count > 20000 {
		return errors.New("SQL exceeds AST limits")
	}
	m, err := decode(raw)
	if err != nil {
		return err
	}
	var allowed string
	switch kind {
	case "CreateStmt":
		allowed = "relation tableElts if_not_exists oncommit"
	case "RangeVar":
		allowed = "schemaname relname inh relpersistence alias"
	case "Alias":
		allowed = "aliasname"
	case "ColumnDef":
		allowed = "colname typeName is_local constraints"
	case "TypeName":
		allowed = "names typemod typmods"
	case "Constraint":
		allowed = "contype conname raw_expr keys pktable fk_attrs pk_attrs fk_matchtype fk_upd_action fk_del_action initially_valid"
	case "AlterTableStmt":
		allowed = "relation cmds objtype missing_ok"
	case "AlterTableCmd":
		allowed = "subtype name def behavior missing_ok"
	case "RenameStmt":
		allowed = "renameType relationType relation subname newname behavior missing_ok"
	case "DropStmt":
		allowed = "objects removeType behavior missing_ok"
	case "IndexStmt":
		allowed = "idxname relation accessMethod indexParams whereClause unique if_not_exists"
	case "IndexElem":
		allowed = "name ordering nulls_ordering"
	case "InsertStmt":
		allowed = "relation cols selectStmt returningList override"
	case "SelectStmt":
		allowed = "valuesLists limitOption op"
	case "UpdateStmt":
		allowed = "relation targetList whereClause returningList"
	case "DeleteStmt":
		allowed = "relation whereClause returningList"
	case "ResTarget":
		allowed = "name val"
	case "List":
		allowed = "items"
	case "A_Const":
		allowed = "ival fval boolval sval isnull"
	case "Integer":
		allowed = "ival"
	case "Float":
		allowed = "fval"
	case "Boolean":
		allowed = "boolval"
	case "String":
		allowed = "sval"
	case "ColumnRef":
		allowed = "fields"
	case "A_Star":
		allowed = ""
	case "A_Expr":
		allowed = "kind name lexpr rexpr"
	case "BoolExpr":
		allowed = "boolop args"
	case "NullTest":
		allowed = "arg nulltesttype"
	case "TypeCast":
		allowed = "arg typeName"
	case "SetToDefault":
		allowed = "typeMod"
	default:
		return fmt.Errorf("unsupported AST node %s", kind)
	}
	if err := fields(m, allowed); err != nil {
		return fmt.Errorf("%s: %w", kind, err)
	}
	switch kind {
	case "CreateStmt":
		if !oneOf(text(m, "oncommit"), "", "ONCOMMIT_NOOP") {
			return errors.New("temporary tables are unsupported")
		}
		if err := v.child(m, "relation", "RangeVar"); err != nil {
			return err
		}
		return v.nodes(m, "tableElts")
	case "RangeVar":
		if !v.policy.schemas[text(m, "schemaname")] || !identifier(text(m, "relname")) || !oneOf(text(m, "relpersistence"), "", "p") {
			return errors.New("relation must use a qualified managed schema")
		}
		return v.child(m, "alias", "Alias")
	case "Alias":
		if !identifier(text(m, "aliasname")) {
			return errors.New("invalid alias")
		}
	case "ColumnDef":
		if name := text(m, "colname"); name != "" && !identifier(name) {
			return errors.New("invalid column")
		}
		if err := v.child(m, "typeName", "TypeName"); err != nil {
			return err
		}
		return v.nodes(m, "constraints")
	case "TypeName":
		names, err := stringNodes(m["names"])
		if err != nil || len(names) < 1 || len(names) > 2 || (len(names) == 2 && names[0] != "pg_catalog") {
			return errors.New("only builtin types are supported")
		}
		if !oneOf(names[len(names)-1], "int2", "int4", "int8", "smallint", "integer", "bigint", "text", "varchar", "bpchar", "bool", "boolean", "numeric", "decimal", "float4", "float8", "real", "date", "time", "timetz", "timestamp", "timestamptz", "uuid", "bytea", "json", "jsonb") {
			return errors.New("unsupported builtin type")
		}
		if n, ok := m["typemod"]; ok && string(n) != "-1" {
			return errors.New("resolved type modifiers are unsupported")
		}
		var mods []json.RawMessage
		if raw := m["typmods"]; raw != nil {
			if err := json.Unmarshal(raw, &mods); err != nil {
				return err
			}
		}
		for _, mod := range mods {
			o, e := decode(mod)
			if e != nil || len(o) != 1 || o["A_Const"] == nil {
				return errors.New("type modifiers must be integer literals")
			}
			c, e := decode(o["A_Const"])
			if e != nil || c["ival"] == nil {
				return errors.New("type modifiers must be integer literals")
			}
			if err := v.node(mod); err != nil {
				return err
			}
		}
	case "Constraint":
		if !oneOf(text(m, "contype"), "CONSTR_NOTNULL", "CONSTR_NULL", "CONSTR_DEFAULT", "CONSTR_CHECK", "CONSTR_PRIMARY", "CONSTR_UNIQUE", "CONSTR_FOREIGN") {
			return errors.New("unsupported constraint")
		}
		if !oneOf(text(m, "fk_matchtype"), "", "s") || !oneOf(text(m, "fk_upd_action"), "", "a", "r", "c", "n", "d") || !oneOf(text(m, "fk_del_action"), "", "a", "r", "c", "n", "d") {
			return errors.New("unsupported foreign key action")
		}
		for _, key := range []string{"keys", "fk_attrs", "pk_attrs"} {
			if m[key] != nil {
				if _, err := stringNodes(m[key]); err != nil {
					return err
				}
			}
		}
		if err := v.child(m, "pktable", "RangeVar"); err != nil {
			return err
		}
		return v.optionalNode(m, "raw_expr")
	case "AlterTableStmt":
		if text(m, "objtype") != "OBJECT_TABLE" {
			return errors.New("only ordinary tables can be altered")
		}
		if err := v.child(m, "relation", "RangeVar"); err != nil {
			return err
		}
		return v.nodes(m, "cmds")
	case "AlterTableCmd":
		if !oneOf(text(m, "behavior"), "", "DROP_RESTRICT") {
			return errors.New("CASCADE is unsupported")
		}
		sub := text(m, "subtype")
		if !oneOf(sub, "AT_AddColumn", "AT_DropColumn", "AT_AlterColumnType", "AT_ColumnDefault", "AT_SetNotNull", "AT_DropNotNull", "AT_AddConstraint", "AT_DropConstraint") {
			return errors.New("unsupported ALTER TABLE action")
		}
		if m["def"] != nil {
			return v.node(m["def"])
		}
	case "RenameStmt":
		if !oneOf(text(m, "renameType"), "OBJECT_COLUMN", "OBJECT_TABLE") || text(m, "relationType") != "OBJECT_TABLE" || !oneOf(text(m, "behavior"), "", "DROP_RESTRICT") || !identifier(text(m, "newname")) {
			return errors.New("unsupported rename")
		}
		return v.child(m, "relation", "RangeVar")
	case "DropStmt":
		if !oneOf(text(m, "removeType"), "OBJECT_TABLE", "OBJECT_INDEX") || !oneOf(text(m, "behavior"), "", "DROP_RESTRICT") {
			return errors.New("only DROP TABLE/INDEX RESTRICT is supported")
		}
		var objects []json.RawMessage
		if err := json.Unmarshal(m["objects"], &objects); err != nil || len(objects) == 0 {
			return errors.New("missing DROP names")
		}
		for _, object := range objects {
			o, e := decode(object)
			if e != nil || len(o) != 1 || o["List"] == nil {
				return errors.New("invalid DROP name")
			}
			list, e := decode(o["List"])
			if e != nil {
				return e
			}
			if e = fields(list, "items"); e != nil {
				return e
			}
			names, e := stringNodes(list["items"])
			if e != nil || len(names) != 2 || !v.policy.schemas[names[0]] {
				return errors.New("DROP name must use a qualified managed schema")
			}
		}
	case "IndexStmt":
		if !oneOf(text(m, "accessMethod"), "", "btree") {
			return errors.New("only ordinary btree indexes are supported")
		}
		if err := v.child(m, "relation", "RangeVar"); err != nil {
			return err
		}
		if err := v.nodes(m, "indexParams"); err != nil {
			return err
		}
		return v.optionalNode(m, "whereClause")
	case "IndexElem":
		if !identifier(text(m, "name")) || !oneOf(text(m, "ordering"), "", "SORTBY_DEFAULT", "SORTBY_ASC", "SORTBY_DESC") || !oneOf(text(m, "nulls_ordering"), "", "SORTBY_NULLS_DEFAULT", "SORTBY_NULLS_FIRST", "SORTBY_NULLS_LAST") {
			return errors.New("unsupported index column")
		}
	case "InsertStmt":
		if !oneOf(text(m, "override"), "", "OVERRIDING_NOT_SET") {
			return errors.New("identity override is unsupported")
		}
		if err := v.child(m, "relation", "RangeVar"); err != nil {
			return err
		}
		if err := v.nodes(m, "cols"); err != nil {
			return err
		}
		if err := v.nodes(m, "returningList"); err != nil {
			return err
		}
		return v.optionalNode(m, "selectStmt")
	case "SelectStmt":
		if m["valuesLists"] == nil || !oneOf(text(m, "op"), "", "SETOP_NONE") || !oneOf(text(m, "limitOption"), "", "LIMIT_OPTION_DEFAULT") {
			return errors.New("only INSERT VALUES is supported")
		}
		return v.nodes(m, "valuesLists")
	case "UpdateStmt", "DeleteStmt":
		if err := v.child(m, "relation", "RangeVar"); err != nil {
			return err
		}
		if err := v.nodes(m, "targetList"); err != nil {
			return err
		}
		if err := v.nodes(m, "returningList"); err != nil {
			return err
		}
		return v.optionalNode(m, "whereClause")
	case "ResTarget":
		return v.optionalNode(m, "val")
	case "List":
		return v.nodes(m, "items")
	case "A_Const":
		for key, childKind := range map[string]string{"ival": "Integer", "fval": "Float", "boolval": "Boolean", "sval": "String"} {
			if err := v.child(m, key, childKind); err != nil {
				return err
			}
		}
	case "ColumnRef":
		var refs []json.RawMessage
		if err := json.Unmarshal(m["fields"], &refs); err != nil || len(refs) == 0 || len(refs) > 3 {
			return errors.New("unsupported column reference")
		}
		for i, ref := range refs {
			o, e := decode(ref)
			if e != nil || len(o) != 1 {
				return errors.New("invalid column reference")
			}
			if o["A_Star"] != nil {
				if i != len(refs)-1 {
					return errors.New("invalid wildcard")
				}
				if err := v.node(ref); err != nil {
					return err
				}
				continue
			}
			if o["String"] == nil {
				return errors.New("unsupported column reference")
			}
			part, e := decode(o["String"])
			if e != nil {
				return e
			}
			if e = fields(part, "sval"); e != nil {
				return e
			}
			if !identifier(text(part, "sval")) || (len(refs) == 3 && i == 0 && !v.policy.schemas[text(part, "sval")]) {
				return errors.New("column reference is outside the managed schema")
			}
		}
	case "A_Expr":
		names, e := stringNodes(m["name"])
		if e != nil || len(names) != 1 || !oneOf(names[0], "+", "-", "*", "/", "%", "=", "<>", "<", ">", "<=", ">=") || text(m, "kind") != "AEXPR_OP" {
			return errors.New("unsupported operator")
		}
		if err := v.optionalNode(m, "lexpr"); err != nil {
			return err
		}
		return v.optionalNode(m, "rexpr")
	case "BoolExpr":
		if !oneOf(text(m, "boolop"), "AND_EXPR", "OR_EXPR", "NOT_EXPR") {
			return errors.New("unsupported boolean expression")
		}
		return v.nodes(m, "args")
	case "NullTest":
		if !oneOf(text(m, "nulltesttype"), "IS_NULL", "IS_NOT_NULL") {
			return errors.New("unsupported NULL test")
		}
		return v.optionalNode(m, "arg")
	case "TypeCast":
		if err := v.child(m, "typeName", "TypeName"); err != nil {
			return err
		}
		return v.optionalNode(m, "arg")
	}
	return nil
}

func (v *validator) optionalNode(m object, key string) error {
	if m[key] == nil {
		return nil
	}
	return v.node(m[key])
}
func (v *validator) child(m object, key, kind string) error {
	if m[key] == nil {
		return nil
	}
	return v.message(kind, m[key])
}
func (v *validator) nodes(m object, key string) error {
	if m[key] == nil {
		return nil
	}
	var nodes []json.RawMessage
	if err := json.Unmarshal(m[key], &nodes); err != nil {
		return err
	}
	for _, node := range nodes {
		if err := v.node(node); err != nil {
			return err
		}
	}
	return nil
}
func decode(raw json.RawMessage) (object, error) {
	var m object
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, errors.New("missing AST object")
	}
	err := json.Unmarshal(raw, &m)
	if err != nil || m == nil {
		return nil, errors.New("invalid AST object")
	}
	return m, nil
}
func fields(m object, allowed string) error {
	for key := range m {
		if key != "location" && !oneOf(key, strings.Fields(allowed)...) {
			return fmt.Errorf("unsupported field %s", key)
		}
	}
	return nil
}
func text(m object, key string) string { var s string; _ = json.Unmarshal(m[key], &s); return s }
func oneOf(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}
func identifier(s string) bool {
	return s != "" && len(s) <= 63 && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
func stringNodes(raw json.RawMessage) ([]string, error) {
	var nodes []json.RawMessage
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return nil, errors.New("invalid identifier list")
	}
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		o, e := decode(node)
		if e != nil || len(o) != 1 || o["String"] == nil {
			return nil, errors.New("invalid identifier")
		}
		s, e := decode(o["String"])
		if e != nil {
			return nil, e
		}
		if e = fields(s, "sval"); e != nil {
			return nil, e
		}
		name := text(s, "sval")
		if !identifier(name) {
			return nil, errors.New("invalid identifier")
		}
		names = append(names, name)
	}
	return names, nil
}
