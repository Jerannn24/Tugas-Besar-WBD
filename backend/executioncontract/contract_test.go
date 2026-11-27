package executioncontract

import (
	"errors"
	"testing"
)

func TestValidateBatchSQL(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  Direction
		sql  string
		want string
	}{
		{"up exact", Up, "UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[])", ""},
		{"up spacing case semicolon", Up, "  update managed.batch_items\n set processed = true  where id = any($1::bigint[]);  ", ""},
		{"down exact", Down, "UPDATE managed.batch_items SET processed = FALSE WHERE id = ANY($1::bigint[]);", ""},
		{"up without where", Up, "UPDATE managed.batch_items SET processed = TRUE", "SQL_UNSUPPORTED"},
		{"up uses down pattern", Up, "UPDATE managed.batch_items SET processed = FALSE WHERE id = ANY($1::bigint[])", "SQL_UNSUPPORTED"},
		{"down uses up pattern", Down, "UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[])", "SQL_UNSUPPORTED"},
		{"other table", Up, "UPDATE managed.items SET processed = TRUE WHERE id = ANY($1::bigint[])", "SQL_UNSUPPORTED"},
		{"stacked statement", Up, "UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]); DROP TABLE x", "SQL_UNSUPPORTED"},
		{"bad direction", Direction("sideways"), "UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[])", "INVALID_DIRECTION"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBatchSQL(tc.dir, tc.sql)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			var ce ContractError
			if !errors.As(err, &ce) || ce.Code != tc.want {
				t.Fatalf("want %s, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateBatchConfig(t *testing.T) {
	ok := BatchConfig{Schema: "managed", Table: "batch_items", CursorColumn: "id", BatchSize: 2}
	if err := ValidateBatchConfig(&ok); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]*BatchConfig{
		"nil":        nil,
		"table":      {Schema: "managed", Table: "items", CursorColumn: "id", BatchSize: 2},
		"cursor":     {Schema: "managed", Table: "batch_items", CursorColumn: "uuid", BatchSize: 2},
		"zero size":  {Schema: "managed", Table: "batch_items", CursorColumn: "id"},
		"large size": {Schema: "managed", Table: "batch_items", CursorColumn: "id", BatchSize: 10001},
	} {
		var ce ContractError
		if err := ValidateBatchConfig(cfg); !errors.As(err, &ce) || ce.Code != "INVALID_BATCH_CONFIG" {
			t.Fatalf("%s: want INVALID_BATCH_CONFIG, got %v", name, err)
		}
	}
}

func TestReversibleSnapshotSQL(t *testing.T) {
	if got := ReversibleSnapshotSQL("src"); got != "length(trim(COALESCE(src.snapshot->>'sql_down',''))) > 0" {
		t.Fatalf("unexpected reversible condition: %s", got)
	}
}
