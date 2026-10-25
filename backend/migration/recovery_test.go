package migration

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestRecoveryTerminalDoesNotOpenTarget(t *testing.T) {
	for _, status := range []string{"succeeded", "failed"} {
		a := sampleAttempt()
		a.Status = status
		s := &memoryStore{a: a}
		h := executionHandler(t, nil, s)
		got, err := h.Reconcile(context.Background(), testDBID, a.ID)
		if err != nil || got.Status != status || s.transitionCalls != 0 {
			t.Fatalf("terminal=%+v %v", got, err)
		}
	}
}

func TestRecoveryReconcilesReceiptWithoutReplayingSQL(t *testing.T) {
	for _, tc := range []string{"matching", "missing", "unreadable", "mismatch", "completed under lock"} {
		t.Run(tc, func(t *testing.T) {
			a := sampleAttempt()
			a.Status = "unknown"
			s := &memoryStore{a: a}
			f := newTargetFixture(t)
			f.expectOpen()
			h := executionHandler(t, f, s)
			if tc == "completed under lock" {
				s.loadFn = func(call int) (attempt, error) {
					if call > 1 {
						a.Status = "succeeded"
					}
					return a, nil
				}
			} else {
				q := f.preparer.ExpectQuery("SELECT execution_id").WithArgs(a.ID)
				switch tc {
				case "missing":
					q.WillReturnRows(sqlmock.NewRows([]string{"id"}))
				case "unreadable":
					q.WillReturnError(errors.New("private receipt failure"))
				case "mismatch":
					r := testReceipt(a)
					r.Version = "999"
					q.WillReturnRows(receiptRows(r))
				default:
					q.WillReturnRows(receiptRows(testReceipt(a)))
				}
			}
			f.expectClose()
			got, err := h.Reconcile(context.Background(), testDBID, a.ID)
			want := "unknown"
			if tc == "matching" || tc == "completed under lock" {
				want = "succeeded"
			}
			if tc == "mismatch" {
				assertCode(t, err, "REQUEST_CONFLICT")
			} else if err != nil {
				t.Fatal(err)
			}
			if got.Status != want || s.loadCalls != 2 || s.reserveCalls != 0 {
				t.Fatalf("recovery=%+v load=%d reserve=%d", got, s.loadCalls, s.reserveCalls)
			}
			// No executor SQL, begin or commit expectations exist: recovery may only
			// acquire/release the lock and read a receipt, never replay the migration.
		})
	}
}

func TestRecoveryRejectsInvalidIDsBeforeStore(t *testing.T) {
	s := &memoryStore{}
	h := executionHandler(t, nil, s)
	_, err := h.Reconcile(context.Background(), "bad", testExecutionID)
	assertCode(t, err, "INVALID_REQUEST")
	if s.loadCalls != 0 {
		t.Fatal("invalid reconciliation touched store")
	}
}

func TestFinishRequiresMatchingSuccessEvidence(t *testing.T) {
	a := sampleAttempt()
	s := &memoryStore{}
	h := executionHandler(t, nil, s)
	got, err := h.finish(context.Background(), a, outcome{status: "succeeded"})
	assertCode(t, err, "REQUEST_CONFLICT")
	if got.Status != "unknown" || got.Error == nil || got.Error.Code != "RECEIPT_MISMATCH" {
		t.Fatalf("unproven success=%+v", got)
	}
}
