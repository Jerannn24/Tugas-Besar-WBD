package viewer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Actor struct{ ID string }
type Access string

const (
	View Access = "view"
	Edit Access = "edit"
)

type Dependencies struct {
	Platform *sql.DB
	Actor    func(*http.Request) (Actor, error)
	// Authorize menolak akses selain view/edit sebelum koneksi target dibuka.
	Authorize func(context.Context, Actor, string, Access) error
	// OpenTarget mengembalikan pool role VIEWER milik provider; handler tidak menutupnya.
	OpenTarget func(context.Context, string) (*sql.DB, error)
	Timeout    time.Duration
}

type Handler struct{ d Dependencies }

func New(d Dependencies) (*Handler, error) {
	if d.Platform == nil || d.Actor == nil || d.Authorize == nil || d.OpenTarget == nil {
		return nil, errors.New("viewer dependencies are required")
	}
	if d.Timeout <= 0 {
		return nil, errors.New("viewer timeout is required")
	}
	return &Handler{d}, nil
}

func (h *Handler) Register(m *http.ServeMux) error {
	if m == nil {
		return errors.New("mux is required")
	}
	m.HandleFunc("GET /api/databases", h.databases)
	m.HandleFunc("GET /api/databases/{database_id}/schemas", h.schemas)
	m.HandleFunc("GET /api/databases/{database_id}/table", h.table)
	m.HandleFunc("GET /api/databases/{database_id}/rows", h.rows)
	return nil
}

func (h *Handler) databases(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.d.Timeout)
	defer cancel()
	rows, err := h.d.Platform.QueryContext(ctx, `SELECT c.id::text,c.name, CASE WHEN bool_or(g.access='edit') THEN 'edit' ELSE 'view' END AS access
FROM app.database_connections c JOIN app.group_database_grants g ON g.database_id=c.id
JOIN app.group_memberships m ON m.group_id=g.group_id
WHERE m.user_id=$1 AND c.deleted_at IS NULL
GROUP BY c.id,c.name ORDER BY c.name,c.id`, a.ID)
	if err != nil {
		internal(w)
		return
	}
	defer rows.Close()
	items := make([]map[string]string, 0)
	for rows.Next() {
		var id, name, access string
		if rows.Scan(&id, &name, &access) != nil {
			internal(w)
			return
		}
		items = append(items, map[string]string{"id": id, "name": name, "access": access})
	}
	if rows.Err() != nil {
		internal(w)
		return
	}
	success(w, map[string]any{"items": items})
}

func (h *Handler) open(w http.ResponseWriter, r *http.Request) (*sql.Tx, context.Context, context.CancelFunc, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), h.d.Timeout)
	a, ok := h.actor(w, r)
	if !ok {
		cancel()
		return nil, nil, nil, false
	}
	id := r.PathValue("database_id")
	if !validID(id) {
		cancel()
		apiError(w, 400, "INVALID_REQUEST", "Invalid database id")
		return nil, nil, nil, false
	}
	if err := h.d.Authorize(ctx, a, id, View); err != nil {
		cancel()
		accessError(w, err)
		return nil, nil, nil, false
	}
	db, err := h.d.OpenTarget(ctx, id)
	if err != nil || db == nil {
		cancel()
		apiError(w, 502, "TARGET_UNAVAILABLE", "Target database is unavailable")
		return nil, nil, nil, false
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		cancel()
		apiError(w, 502, "TARGET_UNAVAILABLE", "Target database is unavailable")
		return nil, nil, nil, false
	}
	// tx mengikat seluruh pembacaan pada koneksi read-only.
	return tx, ctx, cancel, true
}

func (h *Handler) schemas(w http.ResponseWriter, r *http.Request) {
	tx, ctx, cancel, ok := h.open(w, r)
	if !ok {
		return
	}
	defer cancel()
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT n.nspname,c.relname FROM pg_catalog.pg_namespace n LEFT JOIN pg_catalog.pg_class c ON c.relnamespace=n.oid AND c.relkind IN ('r','p') AND has_table_privilege(c.oid,'SELECT')
WHERE has_schema_privilege(n.oid,'USAGE') AND n.nspname NOT IN ('information_schema','wbd_auth') AND left(n.nspname,3)<>'pg_'
ORDER BY n.nspname,c.relname`)
	if err != nil {
		targetError(w)
		return
	}
	defer rows.Close()
	type schema struct {
		Name   string              `json:"name"`
		Tables []map[string]string `json:"tables"`
	}
	by := map[string]*schema{}
	for rows.Next() {
		var s string
		var t sql.NullString
		if rows.Scan(&s, &t) != nil {
			targetError(w)
			return
		}
		if by[s] == nil {
			by[s] = &schema{Name: s, Tables: make([]map[string]string, 0)}
		}
		if t.Valid {
			by[s].Tables = append(by[s].Tables, map[string]string{"name": t.String})
		}
	}
	if rows.Err() != nil {
		targetError(w)
		return
	}
	out := make([]schema, 0, len(by))
	for _, s := range by {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	success(w, map[string]any{"items": out})
}

type Column struct {
	Name     string  `json:"name"`
	Type     string  `json:"data_type"`
	Nullable bool    `json:"nullable"`
	Default  *string `json:"default_expression"`
}
type Constraint struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Definition string `json:"definition"`
}
type Index struct {
	Name       string `json:"name"`
	Definition string `json:"definition"`
	Unique     bool   `json:"is_unique"`
	Primary    bool   `json:"is_primary"`
}
type Metadata struct {
	Schema      string       `json:"schema"`
	Table       string       `json:"table"`
	Columns     []Column     `json:"columns"`
	Constraints []Constraint `json:"constraints"`
	Indexes     []Index      `json:"indexes"`
}

func (h *Handler) table(w http.ResponseWriter, r *http.Request) {
	s, t, ok := params(w, r)
	if !ok {
		return
	}
	tx, ctx, cancel, ok := h.open(w, r)
	if !ok {
		return
	}
	defer cancel()
	defer tx.Rollback()
	meta, err := metadata(tx, ctx, s, t)
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "NOT_FOUND", "Table not found")
		return
	}
	if err != nil {
		targetError(w)
		return
	}
	success(w, meta)
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func metadata(q queryer, ctx context.Context, s, t string) (Metadata, error) {
	m := Metadata{Schema: s, Table: t, Columns: []Column{}, Constraints: []Constraint{}, Indexes: []Index{}}
	var oid int64
	err := q.QueryRowContext(ctx, `SELECT c.oid FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2 AND c.relkind IN ('r','p') AND has_table_privilege(c.oid,'SELECT') AND has_schema_privilege(n.oid,'USAGE') AND n.nspname NOT IN ('information_schema','wbd_auth') AND left(n.nspname,3)<>'pg_'`, s, t).Scan(&oid)
	if err != nil {
		return m, err
	}
	rows, err := q.QueryContext(ctx, `SELECT a.attname,pg_catalog.format_type(a.atttypid,a.atttypmod),NOT a.attnotnull,pg_catalog.pg_get_expr(d.adbin,d.adrelid) FROM pg_catalog.pg_attribute a LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum WHERE a.attrelid=$1 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`, oid)
	if err != nil {
		return m, err
	}
	for rows.Next() {
		var c Column
		var def sql.NullString
		if rows.Scan(&c.Name, &c.Type, &c.Nullable, &def) != nil {
			rows.Close()
			return m, errors.New("scan")
		}
		if def.Valid {
			c.Default = &def.String
		}
		m.Columns = append(m.Columns, c)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return m, err
	}
	rows.Close()
	rows, err = q.QueryContext(ctx, `SELECT conname,CASE contype WHEN 'p' THEN 'primary' WHEN 'f' THEN 'foreign' WHEN 'u' THEN 'unique' WHEN 'c' THEN 'check' WHEN 'x' THEN 'exclusion' ELSE 'other' END,pg_catalog.pg_get_constraintdef(oid,true) FROM pg_catalog.pg_constraint WHERE conrelid=$1 ORDER BY conname`, oid)
	if err != nil {
		return m, err
	}
	for rows.Next() {
		var c Constraint
		if rows.Scan(&c.Name, &c.Kind, &c.Definition) != nil {
			rows.Close()
			return m, errors.New("scan")
		}
		m.Constraints = append(m.Constraints, c)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return m, err
	}
	rows.Close()
	rows, err = q.QueryContext(ctx, `SELECT ci.relname,pg_catalog.pg_get_indexdef(i.indexrelid),i.indisunique,i.indisprimary FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class ci ON ci.oid=i.indexrelid WHERE i.indrelid=$1 ORDER BY ci.relname`, oid)
	if err != nil {
		return m, err
	}
	for rows.Next() {
		var i Index
		if rows.Scan(&i.Name, &i.Definition, &i.Unique, &i.Primary) != nil {
			rows.Close()
			return m, errors.New("scan")
		}
		m.Indexes = append(m.Indexes, i)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return m, err
	}
	rows.Close()
	return m, nil
}

type RowsResult struct {
	Columns []RowColumn `json:"columns"`
	Rows    [][]*string `json:"rows"`
	Limit   int         `json:"limit"`
	Offset  int         `json:"offset"`
	HasMore bool        `json:"has_more"`
}
type RowColumn struct {
	Name string `json:"name"`
	Type string `json:"data_type"`
}

func (h *Handler) rows(w http.ResponseWriter, r *http.Request) {
	s, t, ok := params(w, r)
	if !ok {
		return
	}
	limit, err := queryInt(r, "limit", 50, 1, 200)
	if err != nil {
		apiError(w, 400, "INVALID_REQUEST", "Invalid pagination")
		return
	}
	offset, err := queryInt(r, "offset", 0, 0, int(^uint(0)>>1)-201)
	if err != nil {
		apiError(w, 400, "INVALID_REQUEST", "Invalid pagination")
		return
	}
	tx, ctx, cancel, ok := h.open(w, r)
	if !ok {
		return
	}
	defer cancel()
	defer tx.Rollback()
	meta, err := metadata(tx, ctx, s, t)
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "NOT_FOUND", "Table not found")
		return
	}
	if err != nil {
		targetError(w)
		return
	}
	res := RowsResult{Columns: make([]RowColumn, 0, len(meta.Columns)), Rows: make([][]*string, 0), Limit: limit, Offset: offset}
	for _, c := range meta.Columns {
		res.Columns = append(res.Columns, RowColumn{c.Name, c.Type})
	}
	order, err := orderColumns(tx, ctx, s, t)
	if err != nil {
		targetError(w)
		return
	}
	cols := make([]string, len(meta.Columns))
	for i, c := range meta.Columns {
		cols[i] = "t." + quote(c.Name) + "::text"
	}
	query := "SELECT " + strings.Join(cols, ",") + " FROM " + quote(s) + "." + quote(t) + " AS t"
	if len(order) > 0 {
		query += " ORDER BY "
		for i, c := range order {
			if i > 0 {
				query += ","
			}
			query += "t." + quote(c)
		}
	}
	query += " LIMIT $1 OFFSET $2"
	rows, err := tx.QueryContext(ctx, query, limit+1, offset)
	if err != nil {
		targetError(w)
		return
	}
	defer rows.Close()
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		args := make([]any, len(vals))
		for i := range vals {
			args[i] = &vals[i]
		}
		if rows.Scan(args...) != nil {
			targetError(w)
			return
		}
		if len(res.Rows) == limit {
			res.HasMore = true
			break
		}
		row := make([]*string, len(vals))
		for i, v := range vals {
			if v.Valid {
				x := v.String
				row[i] = &x
			}
		}
		res.Rows = append(res.Rows, row)
	}
	if rows.Err() != nil {
		targetError(w)
		return
	}
	success(w, res)
}
func orderColumns(tx *sql.Tx, ctx context.Context, s, t string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `WITH chosen AS (SELECT i.indrelid,i.indkey,i.indnkeyatts FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid=i.indrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2 AND i.indisunique AND i.indisvalid AND i.indisready AND i.indpred IS NULL AND i.indexprs IS NULL AND NOT EXISTS (SELECT 1 FROM unnest(i.indkey::smallint[]) WITH ORDINALITY AS x(attnum,ord) JOIN pg_catalog.pg_attribute z ON z.attrelid=c.oid AND z.attnum=x.attnum WHERE x.ord<=i.indnkeyatts AND NOT z.attnotnull) ORDER BY i.indisprimary DESC,i.indexrelid LIMIT 1) SELECT a.attname FROM chosen i CROSS JOIN LATERAL unnest(i.indkey::smallint[]) WITH ORDINALITY k(attnum,ord) JOIN pg_catalog.pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum WHERE k.ord<=i.indnkeyatts ORDER BY k.ord`, s, t)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if rows.Scan(&c) != nil {
			return nil, errors.New("scan")
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func params(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	s, t := r.URL.Query().Get("schema"), r.URL.Query().Get("table")
	if !identifier(s) || !identifier(t) || strings.HasPrefix(s, "pg_") || s == "information_schema" || s == "wbd_auth" {
		apiError(w, 400, "INVALID_REQUEST", "Invalid schema or table")
		return "", "", false
	}
	return s, t, true
}
func identifier(s string) bool {
	return s != "" && len(s) <= 63 && !strings.ContainsRune(s, 0) && utf8.ValidString(s)
}
func queryInt(r *http.Request, k string, d, min, max int) (int, error) {
	v := r.URL.Query().Get(k)
	if v == "" {
		return d, nil
	}
	n, e := strconv.Atoi(v)
	if e != nil || n < min || n > max {
		return 0, errors.New("invalid")
	}
	return n, nil
}
func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func validID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
func (h *Handler) actor(w http.ResponseWriter, r *http.Request) (Actor, bool) {
	a, e := h.d.Actor(r)
	if e != nil || a.ID == "" {
		apiError(w, 401, "UNAUTHENTICATED", "Authentication required")
		return Actor{}, false
	}
	return a, true
}
func accessError(w http.ResponseWriter, e error) {
	var httpErr interface{ StatusCode() int }
	if !errors.As(e, &httpErr) {
		internal(w)
		return
	}
	switch httpErr.StatusCode() {
	case 401:
		apiError(w, 401, "UNAUTHENTICATED", "Authentication required")
	case 403:
		apiError(w, 403, "FORBIDDEN", "Access denied")
	case 404:
		apiError(w, 404, "NOT_FOUND", "Database not found")
	default:
		internal(w)
	}
}
func success(w http.ResponseWriter, v any) { write(w, 200, map[string]any{"data": v}) }
func apiError(w http.ResponseWriter, status int, code, msg string) {
	write(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg, "field_errors": map[string]string{}, "retryable": false}})
}
func internal(w http.ResponseWriter) { apiError(w, 500, "INTERNAL_ERROR", "Internal error") }
func targetError(w http.ResponseWriter) {
	apiError(w, 502, "TARGET_UNAVAILABLE", "Target database is unavailable")
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
