package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/drafts"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/history"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/auth"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/connections"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/databaseapi"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/workflow"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

func TestWorkflowRuntimePostgreSQL(t *testing.T) {
	platformURL := workflowTestURL(t, "MIGRATION_TEST_PLATFORM_URL")
	executor := workflowTargetConfig(t, "MIGRATION_TEST_TARGET_EXECUTOR_URL")
	preparer := workflowTargetConfig(t, "MIGRATION_TEST_TARGET_PREPARER_URL")
	viewer := workflowTargetConfig(t, "MIGRATION_TEST_TARGET_VIEWER_URL")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, platformURL)
	if err != nil {
		t.Fatal("invalid MIGRATION_TEST_PLATFORM_URL")
	}
	t.Cleanup(pool.Close)
	platform := databaseapi.OpenPlatform(pool)
	t.Cleanup(func() { _ = platform.Close() })
	target, err := pgx.ConnectConfig(ctx, executor)
	if err != nil {
		t.Fatal("connect to the test target executor")
	}
	t.Cleanup(func() { _ = target.Close(context.Background()) })
	receipts, err := pgx.ConnectConfig(ctx, preparer)
	if err != nil {
		t.Fatal("connect to the test target preparer")
	}
	t.Cleanup(func() { _ = receipts.Close(context.Background()) })

	secret := uuid.NewString()
	suffix := uuid.New().String()
	connection, err := connections.NewRepository(pool, secret).Create(ctx, connections.Input{
		Name: "workflow-smoke-" + suffix, Host: viewer.Host, Port: int(viewer.Port),
		DatabaseName: viewer.Database, Username: viewer.User, Password: viewer.Password, SSLMode: "disable",
		Executor: &connections.RoleCredential{Username: executor.User, Password: &executor.Password},
		Preparer: &connections.RoleCredential{Username: preparer.User, Password: &preparer.Password},
	})
	if err != nil {
		t.Fatalf("register test target: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Release only this binding; keep the immutable audit evidence intact.
		if _, err := pool.Exec(ctx, `UPDATE app.database_connections SET deleted_at=clock_timestamp() WHERE id=$1`, connection.ID); err != nil {
			t.Errorf("retire test connection: %v", err)
		}
	})
	cleanup, err := pgx.ConnectConfig(ctx, workflowTargetConfig(t, "MIGRATION_TEST_TARGET_CLEANUP_URL"))
	if err != nil {
		t.Fatal("connect to the test target cleanup role")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		defer func() { _ = cleanup.Close(ctx) }()
		scoped := "execution_id IN (SELECT execution_id FROM wbd_auth.batch_scopes WHERE database_id=$1)"
		for _, query := range []string{
			"DELETE FROM wbd_auth.batch_row_versions WHERE " + scoped,
			"DELETE FROM wbd_auth.batch_checkpoints WHERE " + scoped,
			"DELETE FROM wbd_auth.batch_tickets WHERE " + scoped,
			"DELETE FROM wbd_auth.batch_scope_items WHERE " + scoped,
			"DELETE FROM wbd_auth.batch_scopes WHERE database_id=$1",
			"DELETE FROM wbd_auth.execution_receipts WHERE database_id=$1",
			"DELETE FROM wbd_auth.execution_tickets WHERE database_id=$1",
		} {
			if _, err := cleanup.Exec(ctx, query, connection.ID); err != nil {
				t.Errorf("cleanup target evidence: %v", err)
			}
		}
	})

	password := "Smoke1!" + uuid.NewString()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	users := auth.NewRepository(pool)
	createUser := func(role string) *auth.User {
		user := &auth.User{Username: role + "-" + suffix, HashPassword: string(hash)}
		if err := users.Create(ctx, user); err != nil {
			t.Fatalf("create %s: %v", role, err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := pool.Exec(ctx, `UPDATE app.users SET is_active=false WHERE id=$1`, user.ID); err != nil {
				t.Errorf("deactivate test %s: %v", role, err)
			}
		})
		return user
	}
	editor, leader := createUser("editor"), createUser("leader")
	groupID := uuid.New()
	table := "managed.workflow_smoke_" + strings.ReplaceAll(suffix, "-", "")
	const title = "Workflow runtime smoke"
	up := "INSERT INTO " + table + " (value) VALUES (42)"
	down := "DELETE FROM " + table + " WHERE value=42"
	if _, err := target.Exec(ctx, "CREATE TABLE "+table+" (value integer NOT NULL)"); err != nil {
		t.Fatalf("create test table: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := target.Exec(ctx, "DROP TABLE "+table); err != nil {
			t.Errorf("drop test table: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `INSERT INTO app.groups (id,name,leader_user_id) VALUES ($1,$2,$3)`, groupID, suffix, leader.ID); err != nil {
		t.Fatalf("create review group: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app.group_memberships (group_id,user_id) VALUES ($1,$2),($1,$3)`, groupID, editor.ID, leader.ID); err != nil {
		t.Fatalf("add group members: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app.group_database_grants (group_id,database_id,access) VALUES ($1,$2,'edit')`, groupID, connection.ID); err != nil {
		t.Fatalf("grant target access: %v", err)
	}

	app, closeRuntime, err := New(ctx, pool, secret)
	if err != nil {
		t.Fatalf("start server runtime: %v", err)
	}
	t.Cleanup(func() {
		if err := closeRuntime(); err != nil {
			t.Errorf("shut down server runtime: %v", err)
		}
	})

	const get, post, patch = http.MethodGet, http.MethodPost, http.MethodPatch
	login := func(user *auth.User) workflowSmokeClient {
		client := workflowSmokeClient{app: app, cookies: make(map[string]*http.Cookie)}
		client.request(t, get, "/api/auth/csrf", nil, http.StatusOK, nil)
		client.request(t, post, "/api/auth/login", auth.LoginRequest{Username: user.Username, Password: password}, http.StatusOK, nil)
		return client
	}
	edit, review := login(editor), login(leader)
	databaseID := connection.ID.String()
	databasePath := "/api/databases/" + databaseID
	var draft drafts.Draft
	edit.request(t, post, databasePath+"/drafts", map[string]string{
		"title": title, "sql": up, "sql_down": down,
	}, http.StatusCreated, &draft)
	if draft.ID == "" || draft.Revision != 1 || draft.SQLDown == nil || *draft.SQLDown != down {
		t.Fatalf("expected newly created reversible draft, got %+v", draft)
	}
	draftPath := databasePath + "/drafts/" + draft.ID
	patchInput := map[string]any{"title": title, "sql": up, "revision": draft.Revision}
	var updated drafts.Draft
	edit.request(t, patch, draftPath, patchInput, http.StatusOK, &updated)
	if updated.Revision != 2 || updated.SQLDown == nil || *updated.SQLDown != down {
		t.Fatalf("patch without sql_down should preserve it, got %+v", updated)
	}
	checkTarget := func(wantCount, wantTotal int) {
		t.Helper()
		var count, total int
		err := target.QueryRow(ctx, "SELECT count(*), COALESCE(sum(value),0) FROM "+table).Scan(&count, &total)
		if err != nil || count != wantCount || total != wantTotal {
			t.Fatalf("target rows=%d sum=%d, want %d and %d: %v", count, total, wantCount, wantTotal, err)
		}
	}
	if code := edit.request(t, post, draftPath+"/execute", nil, http.StatusNotFound, nil); code != "NOT_FOUND" {
		t.Fatalf("direct execute error=%q, want NOT_FOUND", code)
	}
	checkTarget(0, 0)

	var proposal workflow.Proposal
	edit.request(t, post, databasePath+"/proposals", workflow.CreateInput{MigrationID: draft.ID, Revision: updated.Revision, Action: string(executioncontract.Apply)}, http.StatusCreated, &proposal)
	proposalPath := databasePath + "/proposals/" + proposal.ProposalID
	if !strings.Contains(strings.Join(proposal.AllowedActions, ","), "submit_review") {
		t.Fatalf("editor cannot submit draft: %+v", proposal.AllowedActions)
	}
	edit.request(t, post, proposalPath+"/submit-review", map[string]int{"revision": proposal.Revision}, http.StatusOK, &proposal)
	if proposal.Status != string(executioncontract.PendingReview) {
		t.Fatalf("submitted proposal status=%q, want pending_review", proposal.Status)
	}
	if code := edit.request(t, post, proposalPath+"/jobs", workflow.EnqueueInput{RequestID: uuid.NewString(), Revision: proposal.Revision}, http.StatusConflict, nil); code != "APPROVAL_REQUIRED" {
		t.Fatalf("enqueue of pending proposal error=%q, want APPROVAL_REQUIRED", code)
	}
	var reviewView workflow.Proposal
	review.request(t, get, proposalPath, nil, http.StatusOK, &reviewView)
	if !strings.Contains(strings.Join(reviewView.AllowedActions, ","), "approve") {
		t.Fatalf("leader cannot approve pending proposal: %+v", reviewView.AllowedActions)
	}
	review.request(t, post, proposalPath+"/approvals", workflow.ApproveInput{GroupID: groupID.String(), Revision: proposal.Revision}, http.StatusOK, &proposal)
	if proposal.Status != string(executioncontract.Approved) || len(proposal.Approvals) != 1 || proposal.Approvals[0].Status != "approved" {
		t.Fatalf("proposal not approved by required group: status=%q approvals=%+v", proposal.Status, proposal.Approvals)
	}
	if !strings.Contains(strings.Join(proposal.AllowedActions, ","), "enqueue") {
		t.Fatalf("editor cannot enqueue approved proposal: %+v", proposal.AllowedActions)
	}

	queuePath := proposalPath + "/jobs"
	input := workflow.EnqueueInput{RequestID: uuid.NewString(), Revision: proposal.Revision}
	const clientParent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	var responseParent string
	edit.traceparent, edit.tracestate, edit.responseParent = clientParent, "vendor=value", &responseParent
	var accepted workflow.EnqueueOutput
	edit.request(t, post, queuePath, input, http.StatusAccepted, &accepted)
	edit.traceparent, edit.tracestate, edit.responseParent = "", "", nil
	var storedParent, storedState string
	if err := pool.QueryRow(ctx, `SELECT traceparent, tracestate FROM app.jobs WHERE id=$1`, accepted.JobID).Scan(&storedParent, &storedState); err != nil {
		t.Fatalf("read queued trace: %v", err)
	}
	if storedParent != responseParent || storedParent == clientParent || !strings.Contains(storedParent, "4bf92f3577b34da6a3ce929d0e0e4736") || storedState != "vendor=value" {
		t.Fatalf("unexpected queued trace: parent=%q response=%q state=%q", storedParent, responseParent, storedState)
	}
	if accepted.Replayed || accepted.Status != executioncontract.Queued || accepted.JobID == "" || accepted.ExecutionID == "" {
		t.Fatalf("expected newly queued job, got %+v", accepted)
	}
	changedRevision := proposal.Revision + 1
	if code := edit.request(t, post, queuePath, workflow.EnqueueInput{RequestID: input.RequestID, Revision: changedRevision}, http.StatusConflict, nil); code != "REQUEST_CONFLICT" {
		t.Fatalf("reused request with changed revision error=%q, want REQUEST_CONFLICT", code)
	}
	if code := edit.request(t, post, queuePath, workflow.EnqueueInput{RequestID: uuid.NewString(), Revision: changedRevision}, http.StatusConflict, nil); code != "VERSION_CONFLICT" {
		t.Fatalf("new request with stale revision error=%q, want VERSION_CONFLICT", code)
	}
	edit.request(t, post, queuePath, workflow.EnqueueInput{RequestID: uuid.NewString(), Revision: proposal.Revision}, http.StatusConflict, nil)
	job := waitForWorkflowJob(t, ctx, edit, accepted.StatusURL, executioncontract.Succeeded)
	if job.ExecutionID != accepted.ExecutionID || job.ExecutionStatus != executioncontract.Succeeded || job.BlocksDatabase || job.AttemptNo != 1 || job.FinishedAt == nil || job.Error != nil {
		t.Fatalf("unexpected completed job: %+v", job)
	}
	if !slices.Contains(job.AllowedActions, executioncontract.AllowedAction("propose_rollback")) {
		t.Fatalf("editor cannot propose rollback for completed apply: %+v", job.AllowedActions)
	}
	patchInput["revision"] = updated.Revision
	if code := edit.request(t, patch, draftPath, patchInput, http.StatusConflict, nil); code != "ALREADY_APPLIED" {
		t.Fatalf("patch of committed draft error=%q, want ALREADY_APPLIED", code)
	}

	var replay workflow.EnqueueOutput
	edit.request(t, post, queuePath, input, http.StatusOK, &replay)
	if !replay.Replayed || replay.JobAccepted != job.JobAccepted {
		t.Fatalf("replay returned %+v, want original completed job %+v", replay, job.JobAccepted)
	}
	edit.request(t, post, queuePath, workflow.EnqueueInput{RequestID: uuid.NewString(), Revision: proposal.Revision}, http.StatusConflict, nil)
	checkTarget(1, 42)
	var receiptCount, jobCount int
	if err := receipts.QueryRow(ctx, `SELECT count(*) FROM wbd_auth.execution_receipts WHERE database_id=$1 AND execution_id=$2`, connection.ID, accepted.ExecutionID).Scan(&receiptCount); err != nil || receiptCount != 1 {
		t.Fatalf("target receipts=%d, want 1: %v", receiptCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app.jobs WHERE database_id=$1`, connection.ID).Scan(&jobCount); err != nil || jobCount != 1 {
		t.Fatalf("queued jobs=%d, want 1: %v", jobCount, err)
	}

	var list struct {
		ActiveHeadExecutionID *string `json:"active_head_execution_id"`
		Items                 []struct {
			AttemptID      string           `json:"attempt_id"`
			ExecutionID    string           `json:"execution_id"`
			Status         string           `json:"status"`
			IsActiveHead   bool             `json:"is_active_head"`
			AllowedActions []string         `json:"allowed_actions"`
			Actor          history.Executor `json:"actor"`
			Statements     []struct {
				SQL string `json:"sql"`
			} `json:"statements"`
			Commit *executioncontract.HistoryEntry `json:"commit"`
		} `json:"items"`
	}
	edit.request(t, get, databasePath+"/migrations?limit=20", nil, http.StatusOK, &list)
	if len(list.Items) != 1 {
		t.Fatalf("history entries=%d, want 1", len(list.Items))
	}
	entry := list.Items[0]
	if entry.AttemptID == "" || entry.ExecutionID != accepted.ExecutionID || entry.Status != string(executioncontract.Succeeded) ||
		entry.Actor.ID != editor.ID.String() || entry.Actor.Username != editor.Username || entry.Commit == nil ||
		entry.Commit.DatabaseID != databaseID || entry.Commit.DraftID != draft.ID || entry.Commit.DraftRevision != proposal.Revision ||
		entry.Commit.Title != title || entry.Commit.SQL != up || entry.Commit.FinishedAt == nil {
		t.Fatalf("unexpected committed history: %+v", entry)
	}
	if list.ActiveHeadExecutionID == nil || *list.ActiveHeadExecutionID != accepted.ExecutionID ||
		!entry.IsActiveHead || !slices.Contains(entry.AllowedActions, "propose_rollback") {
		t.Fatalf("applied execution should be the active head and allow rollback: head=%v entry=%+v", list.ActiveHeadExecutionID, entry)
	}
	var detail executioncontract.HistoryEntry
	edit.request(t, get, databasePath+"/migrations/"+accepted.ExecutionID, nil, http.StatusOK, &detail)
	if detail.ID != entry.Commit.ID || detail.Status != entry.Commit.Status || detail.Version != entry.Commit.Version || detail.SQL != entry.Commit.SQL {
		t.Fatalf("history detail does not match list entry: %+v", detail)
	}
	var projectedOnce bool
	err = pool.QueryRow(ctx, `SELECT count(*)=1 AND count(*) FILTER (
		WHERE h.execution_id=$2 AND h.job_id=$3 AND h.is_complete AND a.job_id=h.job_id
	) = 1 FROM app.migration_history_entries h LEFT JOIN app.job_attempts a ON a.id=h.attempt_id WHERE h.database_id=$1`,
		connection.ID, accepted.ExecutionID, accepted.JobID).Scan(&projectedOnce)
	if err != nil || !projectedOnce {
		t.Fatalf("expected one complete job-linked history projection, matches=%t: %v", projectedOnce, err)
	}

	var extraApprovals []func(path string, revision int)
	runApproved := func(migrationID string, revision int, action executioncontract.Action, source *string, want executioncontract.JobStatus) executioncontract.JobView {
		t.Helper()
		var next workflow.Proposal
		edit.request(t, post, databasePath+"/proposals", workflow.CreateInput{
			MigrationID: migrationID, Revision: revision, Action: string(action), SourceExecutionID: source,
		}, http.StatusCreated, &next)
		path := databasePath + "/proposals/" + next.ProposalID
		edit.request(t, post, path+"/submit-review", map[string]int{"revision": next.Revision}, http.StatusOK, &next)
		review.request(t, post, path+"/approvals", workflow.ApproveInput{
			GroupID: groupID.String(), Revision: next.Revision,
		}, http.StatusOK, &next)
		for _, approve := range extraApprovals {
			approve(path, next.Revision)
		}
		var queued workflow.EnqueueOutput
		edit.request(t, post, path+"/jobs", workflow.EnqueueInput{RequestID: uuid.NewString(), Revision: next.Revision}, http.StatusAccepted, &queued)
		return waitForWorkflowJob(t, ctx, edit, queued.StatusURL, want)
	}
	rollback := runApproved(draft.ID, updated.Revision, executioncontract.Rollback, &accepted.ExecutionID, executioncontract.Succeeded)
	if rollback.Status != executioncontract.Succeeded {
		t.Fatalf("rollback of job-backed apply did not succeed: %+v (error: %+v)", rollback, rollback.Error)
	}
	checkTarget(0, 0)
	var reverted *string
	var head *string
	if err := pool.QueryRow(ctx, `SELECT h.reverts_execution_id::text, a.active_head_execution_id::text
		FROM app.migration_history_entries h JOIN app.migration_active_heads a USING (database_id)
		WHERE h.database_id=$1 AND h.execution_id=$2`, databaseID, rollback.ExecutionID).Scan(&reverted, &head); err != nil ||
		reverted == nil || *reverted != accepted.ExecutionID || head != nil {
		t.Fatalf("rollback history should link the apply and clear the first head: reverts=%v, head=%v, err=%v", reverted, head, err)
	}
	edit.request(t, get, databasePath+"/migrations?limit=20", nil, http.StatusOK, &list)
	if list.ActiveHeadExecutionID != nil || len(list.Items) != 2 {
		t.Fatalf("rolled-back history should have no active head and two rows: head=%v items=%+v", list.ActiveHeadExecutionID, list.Items)
	}
	for _, item := range list.Items {
		if item.IsActiveHead || (item.ExecutionID == accepted.ExecutionID) != slices.Contains(item.AllowedActions, "propose_forward") ||
			(item.ExecutionID != accepted.ExecutionID && item.ExecutionID != rollback.ExecutionID) {
			t.Fatalf("only the reverted apply should allow forward, with no active head: %+v", list.Items)
		}
	}
	forward := runApproved(draft.ID, updated.Revision, executioncontract.Forward, &accepted.ExecutionID, executioncontract.Succeeded)
	if forward.Status != executioncontract.Succeeded || forward.ExecutionID == accepted.ExecutionID {
		t.Fatalf("forward should succeed as a new execution: %+v (error: %+v)", forward, forward.Error)
	}
	checkTarget(1, 42)

	failedSQL := "INSERT INTO " + table + " (value) VALUES (NULL)"
	edit.request(t, post, databasePath+"/drafts", map[string]string{
		"title": "Fail on target constraint", "sql": failedSQL, "irreversible_reason": "Test only",
	}, http.StatusCreated, &draft)
	failed := runApproved(draft.ID, draft.Revision, executioncontract.Apply, nil, executioncontract.Failed)
	edit.request(t, get, databasePath+"/migrations?limit=20&hide_rollback=true", nil, http.StatusOK, &list)
	if len(list.Items) != 2 || list.Items[0].ExecutionID != failed.ExecutionID ||
		list.Items[0].AttemptID == "" || list.Items[0].Status != "failed" ||
		len(list.Items[0].Statements) == 0 || list.Items[0].Statements[0].SQL != failedSQL ||
		list.Items[1].ExecutionID != forward.ExecutionID {
		t.Fatalf("hide rollback must keep the failed attempt with its SQL and the forward: %+v", list.Items)
	}
	runApproved(forward.MigrationID, forward.Revision, executioncontract.Rollback, &forward.ExecutionID, executioncontract.Succeeded)
	checkTarget(0, 0)

	if os.Getenv("BATCH_SCOPE_ISOLATED_TEST_TARGET") == "1" {
		if _, err := target.Exec(ctx, `CREATE TABLE managed.batch_items (id bigint PRIMARY KEY, processed boolean NOT NULL);
			GRANT SELECT (id, processed) ON managed.batch_items TO wbd_target_a_receipt_owner;
			INSERT INTO managed.batch_items VALUES (1,false),(2,false),(3,false)`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := target.Exec(ctx, `DROP TABLE managed.batch_items`); err != nil {
				t.Error(err)
			}
		})
		edit.request(t, post, databasePath+"/drafts", map[string]any{
			"title": "Batch approval smoke", "mode": executioncontract.Batch,
			"sql":      "UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);",
			"sql_down": "UPDATE managed.batch_items SET processed = FALSE WHERE id = ANY($1::bigint[]);",
			"batch":    executioncontract.BatchConfig{Schema: "managed", Table: "batch_items", CursorColumn: "id", BatchSize: 2},
		}, http.StatusCreated, &draft)
		checkBatch := func(want int) {
			t.Helper()
			var got int
			if err := target.QueryRow(ctx, `SELECT count(*) FROM managed.batch_items WHERE processed`).Scan(&got); err != nil || got != want {
				t.Fatalf("processed=%d, want %d: %v", got, want, err)
			}
		}
		batchConfig := executioncontract.BatchConfig{Schema: "managed", Table: "batch_items", CursorColumn: "id", BatchSize: 2}
		var unsupported drafts.Draft
		edit.request(t, post, databasePath+"/drafts", map[string]any{
			"title": "Batch unsupported pattern", "mode": executioncontract.Batch,
			"sql":      "UPDATE managed.batch_items SET processed = TRUE",
			"sql_down": "UPDATE managed.batch_items SET processed = FALSE WHERE id = ANY($1::bigint[]);",
			"batch":    batchConfig,
		}, http.StatusCreated, &unsupported)
		var rejected workflow.Proposal
		edit.request(t, post, databasePath+"/proposals", workflow.CreateInput{
			MigrationID: unsupported.ID, Revision: unsupported.Revision, Action: string(executioncontract.Apply),
		}, http.StatusCreated, &rejected)
		rejectedPath := databasePath + "/proposals/" + rejected.ProposalID
		if code := edit.request(t, post, rejectedPath+"/submit-review", map[string]int{"revision": rejected.Revision}, http.StatusUnprocessableEntity, nil); code != "SQL_UNSUPPORTED" {
			t.Fatalf("submit of unsupported batch SQL error=%q, want SQL_UNSUPPORTED", code)
		}
		edit.request(t, get, rejectedPath, nil, http.StatusOK, &rejected)
		if rejected.Status != "draft" || rejected.Validation == nil || rejected.Validation.Status != "failed" ||
			len(rejected.Validation.Issues) != 1 || rejected.Validation.Issues[0].Code != "SQL_UNSUPPORTED" {
			t.Fatalf("unsupported batch should stay draft with a SQL_UNSUPPORTED issue: %+v", rejected)
		}
		var unsupportedJobs int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM app.jobs WHERE migration_id=$1`, unsupported.ID).Scan(&unsupportedJobs); err != nil || unsupportedJobs != 0 {
			t.Fatalf("unsupported batch must not reach the queue: jobs=%d err=%v", unsupportedJobs, err)
		}
		outsider := login(createUser("outsider"))
		if code := outsider.request(t, post, rejectedPath+"/submit-review", map[string]int{"revision": rejected.Revision}, http.StatusForbidden, nil); code != "FORBIDDEN" {
			t.Fatalf("submit without access error=%q, want FORBIDDEN", code)
		}
		if code := outsider.request(t, get, databasePath+"/migrations?limit=20", nil, http.StatusForbidden, nil); code != "FORBIDDEN" {
			t.Fatalf("history without access error=%q, want FORBIDDEN", code)
		}

		rowActions := func(client workflowSmokeClient, execution string) []string {
			t.Helper()
			client.request(t, get, databasePath+"/migrations?limit=20", nil, http.StatusOK, &list)
			for _, item := range list.Items {
				if item.ExecutionID == execution && item.Status == string(executioncontract.Succeeded) {
					return item.AllowedActions
				}
			}
			t.Fatalf("execution %s missing from history: %+v", execution, list.Items)
			return nil
		}
		batch := runApproved(draft.ID, draft.Revision, executioncontract.Apply, nil, executioncontract.Succeeded)
		checkBatch(3)
		if !slices.Contains(batch.AllowedActions, executioncontract.AllowedAction("propose_compensate")) {
			t.Fatalf("reversible batch job should offer compensation: %v", batch.AllowedActions)
		}
		if actions := rowActions(edit, batch.ExecutionID); !slices.Contains(actions, "propose_compensate") || slices.Contains(actions, "propose_rollback") {
			t.Fatalf("active batch head should offer compensation only: %v", actions)
		}
		compensation := runApproved(draft.ID, draft.Revision, executioncontract.Compensate, &batch.ExecutionID, executioncontract.Succeeded)
		checkBatch(0)
		if actions := rowActions(edit, batch.ExecutionID); slices.Contains(actions, "propose_compensate") {
			t.Fatalf("compensated batch must not offer compensation again: %v", actions)
		}
		edit.request(t, get, databasePath+"/migrations?limit=20&hide_rollback=true", nil, http.StatusOK, &list)
		for _, item := range list.Items {
			if item.ExecutionID == batch.ExecutionID || item.ExecutionID == compensation.ExecutionID {
				t.Fatalf("fully compensated batch should be hidden: %+v", list.Items)
			}
		}
		forwardBatch := runApproved(draft.ID, draft.Revision, executioncontract.Forward, &batch.ExecutionID, executioncontract.Succeeded)
		checkBatch(3)
		if actions := rowActions(edit, forwardBatch.ExecutionID); !slices.Contains(actions, "propose_compensate") {
			t.Fatalf("forwarded batch head should offer compensation: %v", actions)
		}
		viewer := createUser("viewer")
		viewGroup := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO app.groups (id,name,leader_user_id) VALUES ($1,$2,$3)`, viewGroup, "view-"+suffix, viewer.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO app.group_memberships (group_id,user_id) VALUES ($1,$2)`, viewGroup, viewer.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO app.group_database_grants (group_id,database_id,access) VALUES ($1,$2,'view')`, viewGroup, connection.ID); err != nil {
			t.Fatal(err)
		}
		viewerClient := login(viewer)
		if actions := rowActions(viewerClient, forwardBatch.ExecutionID); len(actions) != 0 {
			t.Fatalf("view-only user must not get batch actions: %v", actions)
		}
		if actions := rowActions(edit, forwardBatch.ExecutionID); !slices.Contains(actions, "propose_compensate") {
			t.Fatalf("editor should keep compensation after roster change: %v", actions)
		}
		extraApprovals = append(extraApprovals, func(path string, revision int) {
			viewerClient.request(t, post, path+"/approvals", workflow.ApproveInput{GroupID: viewGroup.String(), Revision: revision}, http.StatusOK, nil)
		})
		var irreversibleDraft drafts.Draft
		edit.request(t, post, databasePath+"/drafts", map[string]any{
			"title": "Batch without rollback SQL", "mode": executioncontract.Batch,
			"sql":                 "UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);",
			"irreversible_reason": "Test only",
			"batch":               batchConfig,
		}, http.StatusCreated, &irreversibleDraft)
		if _, err := target.Exec(ctx, `UPDATE managed.batch_items SET processed = false`); err != nil {
			t.Fatal(err)
		}
		irreversible := runApproved(irreversibleDraft.ID, irreversibleDraft.Revision, executioncontract.Apply, nil, executioncontract.Succeeded)
		checkBatch(3)
		var head *string
		if err := pool.QueryRow(ctx, `SELECT active_head_execution_id::text FROM app.migration_active_heads WHERE database_id=$1`, connection.ID).Scan(&head); err != nil ||
			head == nil || *head != irreversible.ExecutionID {
			t.Fatalf("irreversible batch apply should be the active head: head=%v err=%v", head, err)
		}
		var irreversibleJob executioncontract.JobView
		edit.request(t, get, irreversible.StatusURL, nil, http.StatusOK, &irreversibleJob)
		if slices.Contains(irreversibleJob.AllowedActions, executioncontract.AllowedAction("propose_compensate")) {
			t.Fatalf("irreversible batch job must not offer compensation: %v", irreversibleJob.AllowedActions)
		}
		if actions := rowActions(edit, irreversible.ExecutionID); slices.Contains(actions, "propose_compensate") {
			t.Fatalf("irreversible batch history row must not offer compensation: %v", actions)
		}
		if code := edit.request(t, post, databasePath+"/proposals", workflow.CreateInput{
			MigrationID: irreversibleDraft.ID, Revision: irreversibleDraft.Revision,
			Action: string(executioncontract.Compensate), SourceExecutionID: &irreversible.ExecutionID,
		}, http.StatusUnprocessableEntity, nil); code != "MIGRATION_IRREVERSIBLE" {
			t.Fatalf("compensate of irreversible batch error=%q, want MIGRATION_IRREVERSIBLE", code)
		}
		if code := outsider.request(t, post, databasePath+"/proposals", workflow.CreateInput{
			MigrationID: irreversibleDraft.ID, Revision: irreversibleDraft.Revision,
			Action: string(executioncontract.Compensate), SourceExecutionID: &irreversible.ExecutionID,
		}, http.StatusForbidden, nil); code != "FORBIDDEN" {
			t.Fatalf("compensate without access error=%q, want FORBIDDEN", code)
		}
		var irreversibleProposals int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM app.migration_proposals WHERE migration_id=$1 AND action='compensate'`, irreversibleDraft.ID).Scan(&irreversibleProposals); err != nil || irreversibleProposals != 0 {
			t.Fatalf("rejected compensation must not create a proposal: count=%d err=%v", irreversibleProposals, err)
		}

		var emptyDraft drafts.Draft
		edit.request(t, post, databasePath+"/drafts", map[string]any{
			"title": "Batch with empty scope", "mode": executioncontract.Batch,
			"sql":      "UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);",
			"sql_down": "UPDATE managed.batch_items SET processed = FALSE WHERE id = ANY($1::bigint[]);",
			"batch":    batchConfig,
		}, http.StatusCreated, &emptyDraft)
		empty := runApproved(emptyDraft.ID, emptyDraft.Revision, executioncontract.Apply, nil, executioncontract.Succeeded)
		if empty.Progress == nil || empty.Progress.TotalRows == nil || *empty.Progress.TotalRows != "0" || empty.Progress.BatchesCommitted != "0" {
			t.Fatalf("empty batch scope should report zero progress: %+v", empty.Progress)
		}
		edit.request(t, get, databasePath+"/migrations?limit=20", nil, http.StatusOK, &list)
		var emptyRows int
		for _, item := range list.Items {
			if item.ExecutionID != empty.ExecutionID {
				continue
			}
			emptyRows++
			if item.AttemptID == "" || item.Status != string(executioncontract.Succeeded) || item.Commit != nil ||
				item.IsActiveHead || len(item.AllowedActions) != 0 || len(item.Statements) != 0 {
				t.Fatalf("empty batch attempt should be listed as succeeded without commit evidence: %+v", item)
			}
		}
		if emptyRows != 1 || list.ActiveHeadExecutionID == nil || *list.ActiveHeadExecutionID != irreversible.ExecutionID {
			t.Fatalf("empty batch should add one attempt row and keep the active head: rows=%d head=%v", emptyRows, list.ActiveHeadExecutionID)
		}
		var emptyEntries int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM app.migration_history_entries WHERE execution_id=$1`, empty.ExecutionID).Scan(&emptyEntries); err != nil || emptyEntries != 0 {
			t.Fatalf("empty batch must not fabricate commit evidence: entries=%d err=%v", emptyEntries, err)
		}
	}
}

func workflowTestURL(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		if os.Getenv("TEST_INTEGRATION_REQUIRED") == "1" {
			t.Fatalf("%s not set", key)
		}
		t.Skipf("%s not set", key)
	}
	return value
}

func workflowTargetConfig(t *testing.T, key string) *pgx.ConnConfig {
	t.Helper()
	cfg, err := pgx.ParseConfig(workflowTestURL(t, key))
	if err != nil {
		t.Fatalf("invalid %s", key)
	}
	return cfg
}

type workflowSmokeClient struct {
	app                     *fiber.App
	cookies                 map[string]*http.Cookie
	traceparent, tracestate string
	responseParent          *string
}

func (c workflowSmokeClient) request(t *testing.T, method, path string, input any, want int, output any) string {
	t.Helper()
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Content-Type", "application/json")
	if c.traceparent != "" {
		req.Header.Set("traceparent", c.traceparent)
		req.Header.Set("tracestate", c.tracestate)
	}
	for _, cookie := range c.cookies {
		req.AddCookie(cookie)
	}
	if csrf := c.cookies[auth.CSRFCookieName]; csrf != nil {
		req.Header.Set("X-CSRF-Token", csrf.Value)
	}
	res, err := c.app.Test(req, 5000)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	if c.responseParent != nil {
		*c.responseParent = res.Header.Get("traceparent")
	}
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != want {
		t.Fatalf("%s %s returned %d, want %d: %s", method, path, res.StatusCode, want, raw)
	}
	for _, cookie := range res.Cookies() {
		c.cookies[cookie.Name] = cookie
	}
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode %s %s: %v", method, path, err)
	}
	if output != nil {
		if err := json.Unmarshal(envelope.Data, output); err != nil {
			t.Fatalf("decode %s %s data: %v", method, path, err)
		}
	}
	return envelope.Error.Code
}

func waitForWorkflowJob(t *testing.T, ctx context.Context, client workflowSmokeClient, statusURL string, want executioncontract.JobStatus) executioncontract.JobView {
	t.Helper()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		var job executioncontract.JobView
		client.request(t, http.MethodGet, statusURL, nil, http.StatusOK, &job)
		if job.Status == want {
			return job
		}
		switch job.Status {
		case executioncontract.Queued, executioncontract.Running, executioncontract.RetryWait:
		default:
			t.Fatalf("job reached %s (execution=%s): %+v", job.Status, job.ExecutionStatus, job.Error)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("waiting for job status %s (last status=%s, error=%+v): %v", want, job.Status, job.Error, ctx.Err())
		}
	}
}
