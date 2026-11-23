package jobs

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/google/uuid"
)

func TestCommitProofSurvivesRecordFailureAndEvidenceMustMatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutation string
	}{
		{name: "valid"},
		{name: "missing", mutation: `DELETE FROM app.job_statement_evidence WHERE attempt_id = $1`},
		{name: "null row count", mutation: `UPDATE app.job_statement_evidence SET row_count = NULL WHERE attempt_id = $1`},
		{name: "mismatched statement", mutation: `UPDATE app.job_statement_evidence SET sql_text = 'SELECT 1' WHERE attempt_id = $1`},
		{name: "mismatched outcome", mutation: `UPDATE app.job_statement_evidence SET outcome = 'failed' WHERE attempt_id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupP3TestEnv(t)
			ctx := context.Background()
			table := "proof_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
			t.Cleanup(func() {
				_, _ = env.targetExecDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS managed."+table)
			})
			if _, err := env.targetExecDB.ExecContext(ctx, "CREATE TABLE managed."+table+" (id bigint primary key)"); err != nil {
				t.Fatal(err)
			}
			statement := "INSERT INTO managed." + table + " (id) VALUES (1)"
			fix := createP3JobFixture(t, env, executioncontract.Apply, executioncontract.Up, statement, "")
			calls, revalidations := 0, 0
			injected := errors.New("injected RecordCommitTx failure")
			gate := p3CommitGate(t, fix, &calls, statement, "1")
			revalidate := gate.RevalidateFunc
			gate.RevalidateFunc = func(ctx context.Context, req executioncontract.RevalidationRequest) (executioncontract.ExecutionPlan, error) {
				revalidations++
				return revalidate(ctx, req)
			}
			record := gate.RecordCommitFunc
			gate.RecordCommitFunc = func(ctx context.Context, tx *sql.Tx, evidence executioncontract.CommitEvidence) error {
				if err := record(ctx, tx, evidence); err != nil {
					return err
				}
				if calls == 1 {
					return injected
				}
				return nil
			}
			deps := ExecutionDeps{Gate: gate, Acquire: env.makeAcquire(t), Timeout: 10 * time.Second}
			outcome, err := New(env.platformDB).ExecuteStandardJob(ctx, fix.claim, deps)
			if err != nil || outcome == nil || outcome.Status != executioncontract.Unknown || !errors.Is(outcome.Error, injected) || calls != 1 || revalidations != 1 {
				t.Fatalf("injected failure: outcome=%+v err=%v commits=%d revalidations=%d", outcome, err, calls, revalidations)
			}
			p3AssertCommitState(t, env, fix, false, executioncontract.Unknown, true)
			var committedAt time.Time
			var xid string
			if err := env.platformDB.QueryRowContext(ctx, `SELECT p.committed_at, p.target_xid FROM app.job_commit_proofs p JOIN app.job_dispatch_authorizations d ON d.job_id = p.job_id AND d.attempt_id = p.attempt_id AND d.target_xid = p.target_xid WHERE p.job_id = $1 AND p.attempt_id = $2`, fix.claim.JobID, fix.claim.AttemptID).Scan(&committedAt, &xid); err != nil || committedAt.IsZero() || xid == "" {
				t.Fatalf("durable proof: timestamp=%v xid=%q err=%v", committedAt, xid, err)
			}
			if tc.mutation != "" {
				res, err := env.ownerDB.ExecContext(ctx, tc.mutation, fix.claim.AttemptID)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := res.RowsAffected(); err != nil || n != 1 {
					t.Fatalf("mutate evidence: rows=%d err=%v", n, err)
				}
			}
			_, _ = env.platformDB.ExecContext(ctx, "UPDATE app.jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1", fix.claim.JobID)
			outcome, err = New(env.platformDB).ReconcileStandardJobByID(ctx, fix.claim.JobID, deps)
			if tc.mutation == "" {
				if err != nil || outcome == nil || outcome.Status != executioncontract.Succeeded || outcome.Error != nil || calls != 2 {
					t.Fatalf("retry: outcome=%+v err=%v commits=%d", outcome, err, calls)
				}
				p3AssertCommitState(t, env, fix, true, executioncontract.Succeeded, false)
			} else {
				if err == nil && (outcome == nil || outcome.Error == nil) {
					t.Fatalf("invalid evidence accepted: outcome=%+v err=%v", outcome, err)
				}
				job, jobErr := New(env.platformDB).GetJob(ctx, fix.dbID, fix.claim.JobID)
				if jobErr != nil || job.Status != executioncontract.Unknown || !job.BlocksDatabase || calls != 1 {
					t.Fatalf("invalid evidence must remain blocked: job=%+v err=%v commits=%d", job, jobErr, calls)
				}
			}
			if revalidations != 1 {
				t.Fatalf("retry revalidated: calls=%d", revalidations)
			}
			var after time.Time
			var afterXID string
			if err := env.platformDB.QueryRowContext(ctx, `SELECT committed_at, target_xid FROM app.job_commit_proofs WHERE job_id = $1 AND attempt_id = $2`, fix.claim.JobID, fix.claim.AttemptID).Scan(&after, &afterXID); err != nil || !after.Equal(committedAt) || afterXID != xid {
				t.Fatalf("proof changed: timestamp=%v xid=%q err=%v", after, afterXID, err)
			}
			var rows, attempts int
			if err := env.targetExecDB.QueryRowContext(ctx, "SELECT count(*) FROM managed."+table).Scan(&rows); err != nil || rows != 1 {
				t.Fatalf("target rows=%d err=%v", rows, err)
			}
			if err := env.platformDB.QueryRowContext(ctx, `SELECT count(*) FROM app.job_attempts WHERE job_id = $1`, fix.claim.JobID).Scan(&attempts); err != nil || attempts != 1 {
				t.Fatalf("attempts=%d err=%v", attempts, err)
			}
		})
	}
}
