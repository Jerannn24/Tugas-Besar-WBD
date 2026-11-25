package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/drafts"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/history"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/jobs"
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
	edit.request(t, post, "/api/proposals", workflow.CreateInput{DatabaseID: databaseID, MigrationID: draft.ID, Action: string(executioncontract.Apply)}, http.StatusCreated, &proposal)
	proposalPath := "/api/proposals/" + proposal.ProposalID
	edit.request(t, post, proposalPath+"/submit", nil, http.StatusOK, &proposal)
	if proposal.Status != string(executioncontract.PendingReview) {
		t.Fatalf("submitted proposal status=%q, want pending_review", proposal.Status)
	}
	review.request(t, post, proposalPath+"/approve", workflow.ApproveInput{GroupID: groupID.String(), Revision: proposal.Revision}, http.StatusOK, &proposal)
	if proposal.Status != string(executioncontract.Approved) || len(proposal.Approvals) != 1 || proposal.Approvals[0].Status != "approved" {
		t.Fatalf("proposal not approved by required group: status=%q approvals=%+v", proposal.Status, proposal.Approvals)
	}

	queuePath := proposalPath + "/queue"
	input := workflow.EnqueueInput{RequestID: uuid.NewString()}
	var accepted workflow.EnqueueOutput
	edit.request(t, post, queuePath, input, http.StatusAccepted, &accepted)
	if accepted.Replayed || accepted.Status != executioncontract.Queued || accepted.JobID == "" || accepted.ExecutionID == "" {
		t.Fatalf("expected newly queued job, got %+v", accepted)
	}
	edit.request(t, post, queuePath, workflow.EnqueueInput{RequestID: uuid.NewString()}, http.StatusConflict, nil)
	job := waitForWorkflowJob(t, ctx, jobs.New(platform), databaseID, accepted.JobID, executioncontract.Succeeded)
	if job.ExecutionID != accepted.ExecutionID || job.ExecutionStatus != executioncontract.Succeeded || job.BlocksDatabase || job.AttemptNo != 1 || job.FinishedAt == nil || job.Error != nil {
		t.Fatalf("unexpected completed job: %+v", job)
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
	edit.request(t, post, queuePath, workflow.EnqueueInput{RequestID: uuid.NewString()}, http.StatusConflict, nil)
	checkTarget(1, 42)
	var receiptCount, jobCount int
	if err := receipts.QueryRow(ctx, `SELECT count(*) FROM wbd_auth.execution_receipts WHERE database_id=$1 AND execution_id=$2`, connection.ID, accepted.ExecutionID).Scan(&receiptCount); err != nil || receiptCount != 1 {
		t.Fatalf("target receipts=%d, want 1: %v", receiptCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app.jobs WHERE database_id=$1`, connection.ID).Scan(&jobCount); err != nil || jobCount != 1 {
		t.Fatalf("queued jobs=%d, want 1: %v", jobCount, err)
	}

	var list struct {
		Items []history.Execution `json:"items"`
	}
	edit.request(t, get, databasePath+"/migrations", nil, http.StatusOK, &list)
	if len(list.Items) != 1 {
		t.Fatalf("history entries=%d, want 1", len(list.Items))
	}
	entry := list.Items[0]
	if entry.ID != accepted.ExecutionID || entry.DatabaseID != databaseID || entry.DraftID != draft.ID || entry.DraftRevision != proposal.Revision ||
		entry.Status != string(executioncontract.Succeeded) || entry.Title != title || entry.SQL != up || entry.FinishedAt == nil || entry.Error != nil ||
		entry.ExecutedBy.ID != editor.ID.String() || entry.ExecutedBy.Username != editor.Username {
		t.Fatalf("unexpected committed history: %+v", entry)
	}
	var detail history.Execution
	edit.request(t, get, databasePath+"/migrations/"+accepted.ExecutionID, nil, http.StatusOK, &detail)
	if detail.ID != entry.ID || detail.Status != entry.Status || detail.Version != entry.Version || detail.SQL != entry.SQL {
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

	runApproved := func(migrationID string, action executioncontract.Action, source *string, want executioncontract.JobStatus) executioncontract.JobView {
		t.Helper()
		var next workflow.Proposal
		edit.request(t, post, "/api/proposals", workflow.CreateInput{
			DatabaseID: databaseID, MigrationID: migrationID, Action: string(action), SourceExecutionID: source,
		}, http.StatusCreated, &next)
		path := "/api/proposals/" + next.ProposalID
		edit.request(t, post, path+"/submit", nil, http.StatusOK, &next)
		review.request(t, post, path+"/approve", workflow.ApproveInput{
			GroupID: groupID.String(), Revision: next.Revision,
		}, http.StatusOK, &next)
		var queued workflow.EnqueueOutput
		edit.request(t, post, path+"/queue", workflow.EnqueueInput{RequestID: uuid.NewString()}, http.StatusAccepted, &queued)
		return waitForWorkflowJob(t, ctx, jobs.New(platform), databaseID, queued.JobID, want)
	}
	rollback := runApproved(draft.ID, executioncontract.Rollback, &accepted.ExecutionID, executioncontract.Succeeded)
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
	forward := runApproved(draft.ID, executioncontract.Forward, &accepted.ExecutionID, executioncontract.Succeeded)
	if forward.Status != executioncontract.Succeeded || forward.ExecutionID == accepted.ExecutionID {
		t.Fatalf("forward should succeed as a new execution: %+v (error: %+v)", forward, forward.Error)
	}
	checkTarget(1, 42)

	failedSQL := "INSERT INTO " + table + " (value) VALUES (NULL)"
	edit.request(t, post, databasePath+"/drafts", map[string]string{
		"title": "Fail on target constraint", "sql": failedSQL, "irreversible_reason": "Test only",
	}, http.StatusCreated, &draft)
	failed := runApproved(draft.ID, executioncontract.Apply, nil, executioncontract.Failed)
	edit.request(t, get, databasePath+"/migrations?hide_rollback=true", nil, http.StatusOK, &list)
	if len(list.Items) != 2 || list.Items[0].ExecutionID == nil || *list.Items[0].ExecutionID != failed.ExecutionID ||
		list.Items[0].AttemptID == nil || list.Items[0].Status != "failed" || list.Items[0].SQL != failedSQL ||
		list.Items[1].ID != forward.ExecutionID {
		t.Fatalf("hide rollback must keep the failed attempt with its SQL and the forward: %+v", list.Items)
	}
	runApproved(forward.MigrationID, executioncontract.Rollback, &forward.ExecutionID, executioncontract.Succeeded)
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
			"batch": executioncontract.BatchConfig{Schema: "managed", Table: "batch_items", CursorColumn: "id", BatchSize: 2},
		}, http.StatusCreated, &draft)
		checkBatch := func(want int) {
			t.Helper()
			var got int
			if err := target.QueryRow(ctx, `SELECT count(*) FROM managed.batch_items WHERE processed`).Scan(&got); err != nil || got != want {
				t.Fatalf("processed=%d, want %d: %v", got, want, err)
			}
		}
		batch := runApproved(draft.ID, executioncontract.Apply, nil, executioncontract.Succeeded)
		checkBatch(3)
		runApproved(draft.ID, executioncontract.Compensate, &batch.ExecutionID, executioncontract.Succeeded)
		checkBatch(0)
		runApproved(draft.ID, executioncontract.Forward, &batch.ExecutionID, executioncontract.Succeeded)
		checkBatch(3)
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
	app     *fiber.App
	cookies map[string]*http.Cookie
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

func waitForWorkflowJob(t *testing.T, ctx context.Context, service *jobs.Service, databaseID, jobID string, want executioncontract.JobStatus) executioncontract.JobView {
	t.Helper()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, err := service.GetJob(ctx, databaseID, jobID)
		if err != nil {
			t.Fatalf("read queued job: %v", err)
		}
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
