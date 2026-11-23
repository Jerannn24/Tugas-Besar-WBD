package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Queryer is implemented by *sql.DB, *sql.Tx, and *sql.Conn.
type Queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// BatchScopeIdentity uniquely identifies an execution's frozen batch scope.
type BatchScopeIdentity struct {
	ExecutionID string
	TargetID    string
	DatabaseID  string
	ActorID     string
}

// BatchScope represents the metadata of a frozen membership scope.
type BatchScope struct {
	ExecutionID string
	TargetID    string
	DatabaseID  string
	ActorID     string
	TotalCount  int64
	Ready       bool
}

// FreezeBatchScope freezes the membership of managed.batch_items server-side for the given execution.
// It is idempotent for the same identity and will not re-snapshot on retry.
// When prep is a transaction, the returned scope is provisional until that transaction commits.
func (s *Service) FreezeBatchScope(ctx context.Context, prep Queryer, ident BatchScopeIdentity) (*BatchScope, error) {
	if ident.ExecutionID == "" || ident.TargetID == "" || ident.DatabaseID == "" || ident.ActorID == "" {
		return nil, errors.New("incomplete batch scope identity")
	}

	var count int64
	err := prep.QueryRowContext(ctx, `
		SELECT wbd_auth.freeze_batch_scope($1::uuid, $2::uuid, $3::uuid, $4::uuid)
	`, ident.ExecutionID, ident.TargetID, ident.DatabaseID, ident.ActorID).Scan(&count)
	if err != nil {
		return nil, fmt.Errorf("freeze batch scope: %w", err)
	}

	return s.GetBatchScope(ctx, prep, ident)
}

// ReadBatchScopeChunk reads a bounded chunk of frozen IDs using cursor pagination.
// It fails closed if the identity is missing, incomplete, or mismatched.
// It does not load the full dataset into Go memory.
func (s *Service) ReadBatchScopeChunk(ctx context.Context, prep Queryer, ident BatchScopeIdentity, afterID *int64, limit int) ([]int64, error) {
	if limit < 1 || limit > 10000 {
		return nil, errors.New("invalid batch chunk limit: must be between 1 and 10000")
	}
	if ident.ExecutionID == "" || ident.TargetID == "" || ident.DatabaseID == "" || ident.ActorID == "" {
		return nil, errors.New("incomplete batch scope identity")
	}

	// Verify ready marker and exact identity matching
	var totalCount int64
	var ready bool
	err := prep.QueryRowContext(ctx, `
		SELECT total_count, ready
		FROM wbd_auth.batch_scopes
		WHERE execution_id = $1::uuid
		  AND physical_target_id = $2::uuid
		  AND database_id = $3::uuid
	`, ident.ExecutionID, ident.TargetID, ident.DatabaseID).Scan(&totalCount, &ready)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("batch scope not found or identity mismatch")
		}
		return nil, fmt.Errorf("verify batch scope identity: %w", err)
	}
	if !ready {
		return nil, errors.New("batch scope is incomplete (not ready)")
	}

	var rows *sql.Rows
	if afterID != nil {
		rows, err = prep.QueryContext(ctx, `
			SELECT item_id
			FROM wbd_auth.batch_scope_items
			WHERE execution_id = $1::uuid AND item_id > $2
			ORDER BY item_id ASC
			LIMIT $3
		`, ident.ExecutionID, *afterID, limit)
	} else {
		rows, err = prep.QueryContext(ctx, `
			SELECT item_id
			FROM wbd_auth.batch_scope_items
			WHERE execution_id = $1::uuid
			ORDER BY item_id ASC
			LIMIT $2
		`, ident.ExecutionID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("read batch scope chunk: %w", err)
	}
	defer rows.Close()

	items := make([]int64, 0, limit)
	for rows.Next() {
		var itemID int64
		if err := rows.Scan(&itemID); err != nil {
			return nil, fmt.Errorf("scan batch item id: %w", err)
		}
		items = append(items, itemID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate batch items: %w", err)
	}

	return items, nil
}

// GetBatchScope reads the metadata for an existing frozen batch scope.
// It fails closed if the identity is missing, incomplete, or mismatched.
func (s *Service) GetBatchScope(ctx context.Context, prep Queryer, ident BatchScopeIdentity) (*BatchScope, error) {
	if ident.ExecutionID == "" || ident.TargetID == "" || ident.DatabaseID == "" || ident.ActorID == "" {
		return nil, errors.New("incomplete batch scope identity")
	}

	var totalCount int64
	var actorID string
	var ready bool
	err := prep.QueryRowContext(ctx, `
		SELECT total_count, ready, actor_id::text
		FROM wbd_auth.batch_scopes
		WHERE execution_id = $1::uuid
		  AND physical_target_id = $2::uuid
		  AND database_id = $3::uuid
	`, ident.ExecutionID, ident.TargetID, ident.DatabaseID).Scan(&totalCount, &ready, &actorID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("batch scope not found or identity mismatch")
		}
		return nil, fmt.Errorf("get batch scope: %w", err)
	}
	if !ready {
		return nil, errors.New("batch scope is incomplete (not ready)")
	}

	return &BatchScope{
		ExecutionID: ident.ExecutionID,
		TargetID:    ident.TargetID,
		DatabaseID:  ident.DatabaseID,
		ActorID:     actorID,
		TotalCount:  totalCount,
		Ready:       ready,
	}, nil
}
