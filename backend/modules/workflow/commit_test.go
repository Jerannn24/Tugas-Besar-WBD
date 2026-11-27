package workflow

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestLiveJobCommitProjection(t *testing.T) {
	dsn := os.Getenv("MIGRATION_TEST_PLATFORM_URL")
	if dsn == "" {
		if os.Getenv("TEST_INTEGRATION_REQUIRED") == "1" {
			t.Fatal("missing MIGRATION_TEST_PLATFORM_URL")
		}
		t.Skip("MIGRATION_TEST_PLATFORM_URL not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	databaseID, targetID, actorID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO app.users(id,username,password_hash) VALUES ($1,$2,'fixture')`, actorID, actorID)
	exec(`INSERT INTO app.migration_target_state (id) VALUES ($1)`, targetID)
	exec(`INSERT INTO app.database_connections
		(id,name,host,port,database_name,username,password_ciphertext,credential_key_id,ssl_mode,target_key,execution_target_id)
		VALUES ($1,$2,'fixture',5432,'fixture','fixture','\x01','fixture','disable',$2,$3)`, databaseID, databaseID, targetID)

	seed := func(mode, version, sqlText string) (executioncontract.CommitEvidence, string) {
		t.Helper()
		e := executioncontract.CommitEvidence{
			JobID: uuid.NewString(), ExecutionID: uuid.NewString(), ProposalID: uuid.NewString(), MigrationID: uuid.NewString(),
			ActorID: actorID, DatabaseID: databaseID, TargetID: targetID, Revision: 1, Version: version,
			Action: executioncontract.Apply, Direction: executioncontract.Up, SQLSnapshot: sqlText, SnapshotHash: "job-snapshot-hash",
			CommittedAt: time.Now().UTC().Truncate(time.Microsecond), RowCount: "1", FinalStep: true,
		}
		e.Receipt = e.ExecutionID
		attemptID := uuid.NewString()
		exec(`INSERT INTO app.migration_drafts (id,database_id,title,sql,created_by) VALUES ($1,$2,'Fixture',$3,$4)`,
			e.MigrationID, e.DatabaseID, e.SQLSnapshot, e.ActorID)
		exec(`INSERT INTO app.migration_proposals (id,database_id,migration_id,revision,action,snapshot,snapshot_hash,created_by)
			VALUES ($1,$2,$3,1,'apply','{}',sha256('proposal'::bytea),$4)`, e.ProposalID, e.DatabaseID, e.MigrationID, e.ActorID)
		exec(`INSERT INTO app.jobs
			(id,database_id,target_id,request_id,proposal_id,migration_id,revision,execution_id,actor_id,action,direction,mode,snapshot,snapshot_hash)
			VALUES ($1,$2,$3,gen_random_uuid()::text,$4,$5,1,$6,$7,'apply','up',$8,'{}',$9)`,
			e.JobID, e.DatabaseID, e.TargetID, e.ProposalID, e.MigrationID, e.ExecutionID, e.ActorID, mode, e.SnapshotHash)
		exec(`INSERT INTO app.job_attempts (id,job_id,execution_id,database_id,migration_id,revision,actor_id,attempt_no,status,sql_entered)
			VALUES ($1,$2,$3,$4,$5,1,$6,1,'running',true)`, attemptID, e.JobID, e.ExecutionID, e.DatabaseID, e.MigrationID, e.ActorID)
		return e, attemptID
	}
	standard, standardAttempt := seed("standard", "10", "UPDATE managed.items SET done = true")
	exec(`INSERT INTO app.job_dispatch_authorizations (job_id,attempt_id,plan,sql_text,sql_sha256,target_xid,target_version)
		VALUES ($1,$2,'{"ActorUsername":"dispatch-user"}',$3,sha256(convert_to($3,'UTF8')),'100',10)`,
		standard.JobID, standardAttempt, standard.SQLSnapshot)
	exec(`INSERT INTO app.job_commit_proofs (job_id,attempt_id,target_xid,committed_at) VALUES ($1,$2,'100',$3)`,
		standard.JobID, standardAttempt, standard.CommittedAt)
	// Later actor or draft changes cannot undo a verified commit.
	exec(`UPDATE app.users SET is_active=false WHERE id=$1`, actorID)
	exec(`UPDATE app.migration_drafts SET revision=2 WHERE id=$1`, standard.MigrationID)

	batch, batchAttempt := seed("batch", "11", "UPDATE managed.items SET done = true WHERE id = ANY($1::bigint[])")
	batchStep := func(index int, final bool) executioncontract.CommitEvidence {
		t.Helper()
		e := batch
		e.FinalStep = final
		e.CommittedAt = batch.CommittedAt.Add(time.Duration(index+1) * time.Second)
		chunkID := strconv.Itoa(index + 10)
		xid := strconv.Itoa(index + 101)
		e.SafeParameters = map[string]any{"ids": []int{index + 10}}
		e.Batch = &executioncontract.BatchCommit{
			JobID: e.JobID, ExecutedBy: executioncontract.Actor{ID: e.ActorID}, RangeStart: &chunkID, RangeEnd: &chunkID,
			SQLSnapshot: e.SQLSnapshot, BatchIndex: strconv.Itoa(index), RowCount: e.RowCount, CommittedAt: e.CommittedAt, Receipt: e.Receipt,
		}
		stepID := uuid.NewString()
		exec(`INSERT INTO app.job_batch_step_authorizations
			(id,job_id,attempt_id,actor_id,job_execution_id,scope_execution_id,physical_target_id,direction,batch_index,ordinal,
			 chunk_ids,plan,sql_text,sql_sha256,target_xid,target_version)
			VALUES ($1,$2,$3,$4,$5,$5,$6,'up',$7,$7+1,$8::jsonb,'{}',$9,sha256(convert_to($9,'UTF8')),$10,11)`,
			stepID, e.JobID, batchAttempt, e.ActorID, e.ExecutionID, e.TargetID, index, "["+chunkID+"]", e.SQLSnapshot, xid)
		exec(`INSERT INTO app.job_batch_commit_proofs
			(step_auth_id,job_id,attempt_id,scope_execution_id,physical_target_id,direction,batch_index,target_xid,row_count,committed_at)
			VALUES ($1,$2,$3,$4,$5,'up',$6,$7,1,$8)`, stepID, e.JobID, batchAttempt, e.ExecutionID, e.TargetID, index, xid, e.CommittedAt)
		return e
	}
	partial, final := batchStep(0, false), batchStep(1, true)
	gate := &Gate{}
	record := func(e executioncontract.CommitEvidence) {
		t.Helper()
		if err := gate.RecordCommitTx(ctx, tx, e); err != nil {
			t.Fatal(err)
		}
	}
	check := func(version int, head string, entries int) {
		t.Helper()
		var gotVersion, gotEntries int
		var gotHead string
		err := tx.QueryRowContext(ctx, `SELECT last_version,
			(SELECT active_head_execution_id::text FROM app.migration_active_heads WHERE database_id=$1),
			(SELECT count(*) FROM app.migration_history_entries WHERE database_id=$1)
			FROM app.migration_history_counters WHERE database_id=$1`, databaseID).Scan(&gotVersion, &gotHead, &gotEntries)
		if err != nil || gotVersion != version || gotHead != head || gotEntries != entries {
			t.Fatalf("counter=%d, head=%q, entries=%d, err=%v; want %d, %s, %d", gotVersion, gotHead, gotEntries, err, version, head, entries)
		}
	}
	bad := standard
	bad.ActorID = uuid.NewString()
	if err := gate.RecordCommitTx(ctx, tx, bad); err == nil {
		t.Fatal("unrelated actor accepted")
	}
	record(standard)
	record(standard)
	check(1, standard.ExecutionID, 1)
	var projectedAttempt, projectedUsername string
	var legacyRows int
	err = tx.QueryRowContext(ctx, `SELECT attempt_id::text,executed_username,
		(SELECT count(*) FROM app.migration_executions WHERE id=$1)
		FROM app.migration_history_entries WHERE execution_id=$1`, standard.ExecutionID).
		Scan(&projectedAttempt, &projectedUsername, &legacyRows)
	if err != nil || projectedAttempt != standardAttempt || projectedUsername != "dispatch-user" || legacyRows != 0 {
		t.Fatalf("attempt=%q, username=%q, legacy rows=%d, err=%v", projectedAttempt, projectedUsername, legacyRows, err)
	}

	record(partial)
	check(2, standard.ExecutionID, 2)
	exec(`SAVEPOINT head_guard`)
	_, err = tx.ExecContext(ctx, `UPDATE app.migration_active_heads SET active_head_execution_id=$1 WHERE database_id=$2`, batch.ExecutionID, databaseID)
	var constraint *pgconn.PgError
	if !errors.As(err, &constraint) || constraint.Code != "23514" {
		t.Fatalf("incomplete head error = %v", err)
	}
	exec(`ROLLBACK TO SAVEPOINT head_guard`)
	bad = partial
	bad.FinalStep = true
	if err := gate.RecordCommitTx(ctx, tx, bad); err == nil {
		t.Fatal("conflicting replay accepted")
	}
	exec(`SAVEPOINT finalization`)
	record(final)
	check(3, batch.ExecutionID, 3)
	exec(`ROLLBACK TO SAVEPOINT finalization`)
	check(2, standard.ExecutionID, 2)
	record(final)
	record(standard)
	check(3, batch.ExecutionID, 3)
}
