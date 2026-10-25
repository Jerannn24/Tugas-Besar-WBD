package databaseapi

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/history"
)

func TestReconcilerScansAtStartupAndPeriodically(t *testing.T) {
	var scans atomic.Int32
	var mu sync.Mutex
	seen := map[string]int{}
	scan := func(context.Context) ([]pendingExecution, error) {
		scans.Add(1)
		return []pendingExecution{{"db-1", "ex-1"}, {"db-2", "ex-2"}}, nil
	}
	reconcile := func(_ context.Context, databaseID, executionID string) (history.Execution, error) {
		mu.Lock()
		seen[databaseID+"/"+executionID]++
		mu.Unlock()
		if executionID == "ex-2" {
			return history.Execution{Status: "unknown"}, errors.New("target unavailable")
		}
		return history.Execution{Status: "succeeded"}, nil
	}
	stop := startReconciler(scan, reconcile, 20*time.Millisecond)
	time.Sleep(70 * time.Millisecond)
	stop()
	stop()
	after := scans.Load()
	time.Sleep(50 * time.Millisecond)
	if after < 2 {
		t.Fatalf("scans = %d, want startup scan plus at least one tick", after)
	}
	if scans.Load() != after {
		t.Fatal("reconciler kept running after stop")
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["db-1/ex-1"] < 2 || seen["db-2/ex-2"] < 2 {
		t.Fatalf("unexpected reconcile calls: %v", seen)
	}
}

func TestReconcilePendingStopsOnCancelAndScanError(t *testing.T) {
	calls := 0
	reconcile := func(context.Context, string, string) (history.Execution, error) {
		calls++
		return history.Execution{}, nil
	}
	reconcilePending(context.Background(), func(context.Context) ([]pendingExecution, error) {
		return nil, errors.New("down")
	}, reconcile)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reconcilePending(ctx, func(context.Context) ([]pendingExecution, error) {
		return []pendingExecution{{"db", "ex"}}, nil
	}, reconcile)
	if calls != 0 {
		t.Fatalf("reconcile called %d times, want 0", calls)
	}
}
