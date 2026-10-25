package databaseapi

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/history"
)

type pendingExecution struct{ databaseID, executionID string }

type scanFunc func(ctx context.Context) ([]pendingExecution, error)

type reconcileFunc func(ctx context.Context, databaseID, executionID string) (history.Execution, error)

func scanUnresolved(platform *sql.DB, batch int) scanFunc {
	return func(ctx context.Context) ([]pendingExecution, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		rows, err := platform.QueryContext(ctx, `SELECT database_id::text, id::text
FROM app.migration_executions
WHERE status IN ('running', 'unknown')
ORDER BY started_at
LIMIT $1`, batch)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var items []pendingExecution
		for rows.Next() {
			var p pendingExecution
			if err := rows.Scan(&p.databaseID, &p.executionID); err != nil {
				return nil, err
			}
			items = append(items, p)
		}
		return items, rows.Err()
	}
}

// startReconciler memindai execution running/unknown saat startup lalu berkala,
// tanpa tick yang tumpang tindih. Hanya jalur server tepercaya; SQL tidak dijalankan ulang.
func startReconciler(scan scanFunc, reconcile reconcileFunc, interval time.Duration) func() {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			reconcilePending(ctx, scan, reconcile)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			wg.Wait()
		})
	}
}

func reconcilePending(ctx context.Context, scan scanFunc, reconcile reconcileFunc) {
	items, err := scan(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("reconciler: scan failed")
		}
		return
	}
	unresolved := 0
	for _, p := range items {
		if ctx.Err() != nil {
			return
		}
		execution, err := reconcile(ctx, p.databaseID, p.executionID)
		if err != nil || (execution.Status != "succeeded" && execution.Status != "failed") {
			unresolved++
		}
	}
	if unresolved > 0 {
		log.Printf("reconciler: %d execution(s) still unresolved", unresolved)
	}
}
