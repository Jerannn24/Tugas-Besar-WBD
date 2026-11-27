package jobs

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/google/uuid"
)

func TestRequestConflictErrorIdentity(t *testing.T) {
	err := makeRequestConflict("different request")
	if !errors.Is(err, executioncontract.ErrRequestConflict) {
		t.Fatalf("errors.Is did not find request conflict: %v", err)
	}
	var contractErr executioncontract.ContractError
	if !errors.As(err, &contractErr) || contractErr.Code != "REQUEST_CONFLICT" || contractErr.Message != "different request" {
		t.Fatalf("errors.As = %+v, error %v", contractErr, err)
	}
}

func TestListCursorValidation(t *testing.T) {
	db := uuid.NewString()
	id := uuid.NewString()
	for _, tc := range []struct { name, db, kind string; page executioncontract.Page; valid bool }{
		{"default", db, "jobs", executioncontract.Page{}, true},
		{"maximum", db, "jobs", executioncontract.Page{Limit: 100}, true},
		{"negative", db, "jobs", executioncontract.Page{Limit: -1}, false},
		{"too large", db, "jobs", executioncontract.Page{Limit: 101}, false},
		{"invalid database", "bad", "jobs", executioncontract.Page{}, false},
		{"valid cursor", db, "jobs", executioncontract.Page{Cursor: stringPtr(encodeListCursor(db, "jobs", time.Now(), id))}, true},
		{"foreign database", db, "jobs", executioncontract.Page{Cursor: stringPtr(encodeListCursor(uuid.NewString(), "jobs", time.Now(), id))}, false},
		{"wrong kind", db, "jobs", executioncontract.Page{Cursor: stringPtr(encodeListCursor(db, "attempts", time.Now(), id))}, false},
		{"bad cursor", db, "jobs", executioncontract.Page{Cursor: stringPtr("not a cursor")}, false},
		{"bad id", db, "jobs", executioncontract.Page{Cursor: stringPtr(encodeListCursor(db, "jobs", time.Now(), "bad"))}, false},
		{"missing time", db, "jobs", executioncontract.Page{Cursor: stringPtr(encodeListCursor(db, "jobs", time.Time{}, id))}, false},
		{"empty cursor", db, "jobs", executioncontract.Page{Cursor: stringPtr("")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := listCursor(tc.db, tc.kind, tc.page)
			if tc.valid && err != nil { t.Fatal(err) }
			if !tc.valid {
				var ce executioncontract.ContractError
				if !errors.As(err, &ce) || ce.Code != "INVALID_REQUEST" { t.Fatalf("expected INVALID_REQUEST, got %v", err) }
			}
		})
	}
}

func stringPtr(s string) *string { return &s }

func TestListsScopedStablePages(t *testing.T) {
	platform, owner := testDBs(t)
	f := createFixture(t, platform, owner)
	other := createFixture(t, platform, owner)
	svc := New(platform)
	ctx := context.Background()
	var jobs, attempts []string
	for i := 0; i < 3; i++ {
		g := f
		if i > 0 {
			g = createFixtureMigration(t, ctx, platform, f)
		}
		tx, err := platform.BeginTx(ctx, nil)
		if err != nil { t.Fatal(err) }
		res, err := svc.EnqueueTx(ctx, tx, executioncontract.EnqueueRequest{
			DatabaseID: f.dbID, TargetID: f.targetID, RequestID: uuid.NewString(), ProposalID: g.proposalID,
			MigrationID: g.draftID, Revision: 1, ActorID: f.actorID, Action: executioncontract.Apply,
			Direction: executioncontract.Up, Snapshot: g.snapshot, SnapshotHash: f.hash,
		})
		if err != nil { tx.Rollback(); t.Fatal(err) }
		if err := tx.Commit(); err != nil { t.Fatal(err) }
		at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		if i == 2 { at = at.Add(time.Hour) }
		if _, err := owner.ExecContext(ctx, `UPDATE app.jobs SET created_at = $2 WHERE id = $1`, res.JobID, at); err != nil { t.Fatal(err) }
		jobs = append(jobs, res.JobID)
		id := uuid.NewString()
		_, err = owner.ExecContext(ctx, `INSERT INTO app.job_attempts
			(id, job_id, execution_id, database_id, migration_id, revision, actor_id, attempt_no, status, error_code, error_message, error_retryable, started_at)
			VALUES ($1,$2,$3,$4,$5,1,$6,1,'failed','TEST','failure',true,$7)`, id, res.JobID, res.ExecutionID, f.dbID, g.draftID, f.actorID, at)
		if err != nil { t.Fatal(err) }
		attempts = append(attempts, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(jobs[:2])))
	sort.Sort(sort.Reverse(sort.StringSlice(attempts[:2])))
	jobs = append([]string{jobs[2]}, jobs[:2]...)
	attempts = append([]string{attempts[2]}, attempts[:2]...)
	var gotJobs, gotAttempts []string
	var cursor *string
	for {
		p, err := svc.ListJobs(ctx, f.dbID, executioncontract.Page{Limit: 1, Cursor: cursor})
		if err != nil { t.Fatal(err) }
		for _, item := range p.Items {
			gotJobs = append(gotJobs, item.JobID)
			view, err := svc.GetJob(ctx, f.dbID, item.JobID)
			if err != nil || !reflect.DeepEqual(item, view) { t.Fatalf("list mapping differs from GetJob: %+v %+v %v", item, view, err) }
		}
		if p.NextCursor == nil { break }
		cursor = p.NextCursor
	}
	if !reflect.DeepEqual(gotJobs, jobs) { t.Fatalf("jobs = %v, want %v", gotJobs, jobs) }
	cursor = nil
	for {
		p, err := svc.ListAttempts(ctx, f.dbID, executioncontract.Page{Limit: 1, Cursor: cursor})
		if err != nil { t.Fatal(err) }
		for _, item := range p.Items {
			gotAttempts = append(gotAttempts, item.AttemptID)
			if item.DatabaseID != f.dbID || item.JobID == nil || item.Error == nil || item.Error.Code != "TEST" || !item.Error.Retryable { t.Fatalf("attempt mapping: %+v", item) }
		}
		if p.NextCursor == nil { break }
		cursor = p.NextCursor
	}
	if !reflect.DeepEqual(gotAttempts, attempts) { t.Fatalf("attempts = %v, want %v", gotAttempts, attempts) }
	jp, err := svc.ListJobs(ctx, other.dbID, executioncontract.Page{})
	if err != nil || jp.Items == nil || len(jp.Items) != 0 || jp.NextCursor != nil { t.Fatalf("empty jobs: %+v %v", jp, err) }
	ap, err := svc.ListAttempts(ctx, other.dbID, executioncontract.Page{})
	if err != nil || ap.Items == nil || len(ap.Items) != 0 || ap.NextCursor != nil { t.Fatalf("empty attempts: %+v %v", ap, err) }
	foreign := encodeListCursor(f.dbID, "jobs", time.Now(), jobs[0])
	if _, err := svc.ListJobs(ctx, other.dbID, executioncontract.Page{Cursor: &foreign}); err == nil { t.Fatal("foreign cursor accepted") }
}
