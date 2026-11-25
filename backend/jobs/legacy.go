package jobs

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
)

// guardLegacyUnresolved must run under the dispatcher's target advisory lock.
func guardLegacyUnresolved(ctx context.Context, tx *sql.Tx, targetID string) error {
	var blocked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM app.migration_executions
		WHERE execution_target_id = $1 AND status IN ('running', 'unknown')
	)`, targetID).Scan(&blocked); err != nil {
		return fmt.Errorf("check legacy target block: %w", err)
	}
	if blocked {
		return executioncontract.ContractError{Code: "TARGET_BLOCKED", Message: "target has an unresolved execution"}
	}
	return nil
}
