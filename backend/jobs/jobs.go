package jobs

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/google/uuid"
)

type requestConflictError struct {
	executioncontract.ContractError
}

func (e requestConflictError) Is(target error) bool {
	return target == executioncontract.ErrRequestConflict
}

func (e requestConflictError) Unwrap() error {
	return e.ContractError
}

func makeRequestConflict(msg string) error {
	return requestConflictError{
		ContractError: executioncontract.ContractError{
			Code:    "REQUEST_CONFLICT",
			Message: msg,
		},
	}
}

func sameID(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

type Service struct {
	db *sql.DB
}

func New(db *sql.DB) *Service {
	return &Service{db: db}
}

var _ executioncontract.Jobs = (*Service)(nil)

func (s *Service) EnqueueTx(
	ctx context.Context,
	tx *sql.Tx,
	in executioncontract.EnqueueRequest,
) (executioncontract.EnqueueResult, error) {
	if tx == nil {
		return executioncontract.EnqueueResult{}, errors.New("platform transaction is required")
	}
	if in.DatabaseID == "" || in.RequestID == "" || in.ProposalID == "" || in.MigrationID == "" || in.ActorID == "" || in.TargetID == "" {
		return executioncontract.EnqueueResult{}, executioncontract.ContractError{
			Code:    "INVALID_REQUEST",
			Message: "missing required job fields",
		}
	}
	if in.Action == executioncontract.Resume || in.Action == executioncontract.Compensate {
		if in.SourceExecutionID == nil || *in.SourceExecutionID == "" {
			return executioncontract.EnqueueResult{}, makeRequestConflict(fmt.Sprintf("%s requires source execution id", in.Action))
		}
	}

	direction := in.Direction
	if direction == "" {
		if in.Action == executioncontract.Rollback || in.Action == executioncontract.Compensate {
			direction = executioncontract.Down
		} else {
			direction = executioncontract.Up
		}
	}

	// Lock the connection before looking up the request: a missing job row cannot be locked.
	var boundTarget sql.NullString
	bindingErr := tx.QueryRowContext(ctx, `
		SELECT execution_target_id::text
		FROM app.database_connections
		WHERE id = $1 AND deleted_at IS NULL
		FOR UPDATE
	`, in.DatabaseID).Scan(&boundTarget)
	if bindingErr != nil && !errors.Is(bindingErr, sql.ErrNoRows) {
		return executioncontract.EnqueueResult{}, fmt.Errorf("query database binding: %w", bindingErr)
	}

	var (
		oldID                  string
		oldExecutionID         string
		oldProposalID          string
		oldDatabaseID          string
		oldTargetID            string
		oldActorID             string
		oldMigrationID         string
		oldRevision            int
		oldAction              string
		oldDirection           string
		oldSourceExecutionID   *string
		oldSnapshotHash        string
		oldApprovalFingerprint *string
		oldStatus              string
	)

	err := tx.QueryRowContext(ctx, `
		SELECT id, execution_id, proposal_id, database_id, target_id, actor_id,
		       migration_id, revision, action, direction, source_execution_id,
		       snapshot_hash, approval_fingerprint, status
		FROM app.jobs
		WHERE database_id = $1 AND request_id = $2
	`, in.DatabaseID, in.RequestID).Scan(
		&oldID, &oldExecutionID, &oldProposalID, &oldDatabaseID, &oldTargetID, &oldActorID,
		&oldMigrationID, &oldRevision, &oldAction, &oldDirection, &oldSourceExecutionID,
		&oldSnapshotHash, &oldApprovalFingerprint, &oldStatus,
	)

	if err == nil {
		oldFingerprint := ""
		if oldApprovalFingerprint != nil {
			oldFingerprint = *oldApprovalFingerprint
		}
		if oldActorID != in.ActorID ||
			oldTargetID != in.TargetID ||
			oldMigrationID != in.MigrationID ||
			oldProposalID != in.ProposalID ||
			oldRevision != in.Revision ||
			oldAction != string(in.Action) ||
			oldDirection != string(direction) ||
			oldSnapshotHash != in.SnapshotHash ||
			oldFingerprint != in.ApprovalFingerprint ||
			!sameID(oldSourceExecutionID, in.SourceExecutionID) {
			return executioncontract.EnqueueResult{}, makeRequestConflict("request intent differs from existing request")
		}

		return executioncontract.EnqueueResult{
			JobAccepted: executioncontract.JobAccepted{
				JobID:       oldID,
				ExecutionID: oldExecutionID,
				ProposalID:  oldProposalID,
				DatabaseID:  oldDatabaseID,
				Status:      executioncontract.JobStatus(oldStatus),
				StatusURL:   "/api/databases/" + oldDatabaseID + "/jobs/" + oldID,
			},
			Replayed: true,
		}, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return executioncontract.EnqueueResult{}, fmt.Errorf("query existing job: %w", err)
	}

	if errors.Is(bindingErr, sql.ErrNoRows) {
		return executioncontract.EnqueueResult{}, executioncontract.ContractError{
			Code:    "NOT_FOUND",
			Message: "database not found",
		}
	}
	if !boundTarget.Valid || boundTarget.String != in.TargetID {
		return executioncontract.EnqueueResult{}, executioncontract.ContractError{
			Code:    "DATABASE_BUSY",
			Message: "target binding has changed",
		}
	}
	// Serialize proposal admission with workflow review and other enqueues.
	// Snapshot encodings belong to the proposal producer.
	var proposalDatabaseID, proposalMigrationID, proposalAction, proposalStatus string
	var proposalRevision int
	var proposalSourceExecutionID *string
	err = tx.QueryRowContext(ctx, `
		SELECT database_id::text, migration_id::text, revision, action,
		       source_execution_id::text, status
		FROM app.migration_proposals
		WHERE id = $1
		FOR UPDATE
	`, in.ProposalID).Scan(&proposalDatabaseID, &proposalMigrationID, &proposalRevision,
		&proposalAction, &proposalSourceExecutionID, &proposalStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return executioncontract.EnqueueResult{}, executioncontract.ContractError{
			Code:    "NOT_FOUND",
			Message: "proposal not found",
		}
	}
	if err != nil {
		return executioncontract.EnqueueResult{}, fmt.Errorf("query proposal: %w", err)
	}
	if proposalDatabaseID != in.DatabaseID || proposalMigrationID != in.MigrationID ||
		proposalRevision != in.Revision || proposalAction != string(in.Action) ||
		!sameID(proposalSourceExecutionID, in.SourceExecutionID) {
		return executioncontract.EnqueueResult{}, makeRequestConflict("request intent differs from proposal")
	}

	// Only a stale, pre-dispatch failure followed by a genuinely new approved
	// round permits another job. Every other prior job remains a blocker.
	var blocked bool
	err = tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM app.jobs j
			WHERE j.proposal_id = $1 AND (
				j.status = 'failed' AND j.execution_status = 'failed'
				AND j.error_code = 'REVIEW_STALE' AND NOT j.blocks_database
				AND $2 = 'approved'
				AND EXISTS (
					SELECT 1 FROM app.migration_proposal_approvals a
					WHERE a.proposal_id = j.proposal_id
					  AND a.approval_round = (SELECT max(x.approval_round) FROM app.migration_proposal_approvals x WHERE x.proposal_id = j.proposal_id)
					  AND a.created_at > j.created_at
				)
				AND NOT EXISTS (
					SELECT 1 FROM app.migration_proposal_approvals a
					WHERE a.proposal_id = j.proposal_id
					  AND a.approval_round = (SELECT max(x.approval_round) FROM app.migration_proposal_approvals x WHERE x.proposal_id = j.proposal_id)
					  AND (a.status <> 'approved' OR a.created_at <= j.created_at)
				)
				AND NOT EXISTS (SELECT 1 FROM app.job_dispatch_authorizations a WHERE a.job_id = j.id)
				AND NOT EXISTS (SELECT 1 FROM app.job_batch_step_authorizations a WHERE a.job_id = j.id)
				AND NOT EXISTS (
					SELECT 1 FROM app.job_attempts a WHERE a.job_id = j.id
					  AND (a.sql_entered OR EXISTS (SELECT 1 FROM app.job_statement_evidence e WHERE e.attempt_id = a.id))
				)
			) IS NOT TRUE
		)
	`, in.ProposalID, proposalStatus).Scan(&blocked)
	if err != nil {
		return executioncontract.EnqueueResult{}, fmt.Errorf("query prior proposal jobs: %w", err)
	}
	if blocked {
		return executioncontract.EnqueueResult{}, makeRequestConflict("proposal already has an execution job")
	}

	jobID := uuid.NewString()
	var executionID string
	if in.Action == executioncontract.Resume {
		executionID = *in.SourceExecutionID
	} else {
		executionID = uuid.NewString()
	}

	mode := in.Snapshot.Mode
	if mode == "" {
		mode = executioncontract.Standard
	}

	snapshotBytes, err := json.Marshal(in.Snapshot)
	if err != nil {
		return executioncontract.EnqueueResult{}, fmt.Errorf("marshal snapshot: %w", err)
	}

	title := in.Snapshot.Title

	var approvalFP *string
	if in.ApprovalFingerprint != "" {
		approvalFP = &in.ApprovalFingerprint
	}
	var traceparent *string
	if in.Traceparent != "" {
		traceparent = &in.Traceparent
	}
	var tracestate *string
	if in.Tracestate != "" {
		tracestate = &in.Tracestate
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO app.jobs (
			id, database_id, target_id, request_id, proposal_id, migration_id,
			revision, execution_id, actor_id, action, direction, mode,
			source_execution_id, status, execution_status, attempt_no, blocks_database,
			snapshot, snapshot_hash, approval_fingerprint, title, traceparent, tracestate,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10, $11, $12,
			$13, $14, $15, $16, $17,
			$18, $19, $20, $21, $22, $23,
			clock_timestamp(), clock_timestamp()
		)
	`,
		jobID, in.DatabaseID, in.TargetID, in.RequestID, in.ProposalID, in.MigrationID,
		in.Revision, executionID, in.ActorID, string(in.Action), string(direction), string(mode),
		in.SourceExecutionID, string(executioncontract.Queued), string(executioncontract.Queued), 1, false,
		snapshotBytes, in.SnapshotHash, approvalFP, title, traceparent, tracestate,
	)
	if err != nil {
		return executioncontract.EnqueueResult{}, fmt.Errorf("insert job: %w", err)
	}

	return executioncontract.EnqueueResult{
		JobAccepted: executioncontract.JobAccepted{
			JobID:       jobID,
			ExecutionID: executionID,
			ProposalID:  in.ProposalID,
			DatabaseID:  in.DatabaseID,
			Status:      executioncontract.Queued,
			StatusURL:   "/api/databases/" + in.DatabaseID + "/jobs/" + jobID,
		},
		Replayed: false,
	}, nil
}

func (s *Service) GetJob(
	ctx context.Context,
	databaseID string,
	jobID string,
) (executioncontract.JobView, error) {
	if s.db == nil {
		return executioncontract.JobView{}, errors.New("db is required")
	}
	if databaseID == "" || jobID == "" {
		return executioncontract.JobView{}, executioncontract.ContractError{
			Code:    "INVALID_REQUEST",
			Message: "database_id and job_id are required",
		}
	}

	var (
		id                  string
		dbID                string
		targetID            string
		requestID           string
		proposalID          string
		migrationID         string
		revision            int
		executionID         string
		actorID             string
		action              string
		direction           string
		mode                string
		sourceExecID        *string
		status              string
		execStatus          string
		attemptNo           int
		blocksDatabase      bool
		snapshotBytes       []byte
		snapshotHash        string
		approvalFingerprint *string
		title               string
		traceparent         *string
		tracestate          *string
		progressBytes       []byte
		errorCode           *string
		errorMessage        *string
		errorRetryable      *bool
		startedAt           *time.Time
		finishedAt          *time.Time
		nextRetryAt         *time.Time
		createdAt           time.Time
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT
			id, database_id, target_id, request_id, proposal_id, migration_id,
			revision, execution_id, actor_id, action, direction, mode,
			source_execution_id, status, execution_status, attempt_no, blocks_database,
			snapshot, snapshot_hash, approval_fingerprint, title, traceparent, tracestate,
			progress, error_code, error_message, error_retryable,
			started_at, finished_at, next_retry_at, created_at
		FROM app.jobs
		WHERE database_id = $1 AND id = $2
	`, databaseID, jobID).Scan(
		&id, &dbID, &targetID, &requestID, &proposalID, &migrationID,
		&revision, &executionID, &actorID, &action, &direction, &mode,
		&sourceExecID, &status, &execStatus, &attemptNo, &blocksDatabase,
		&snapshotBytes, &snapshotHash, &approvalFingerprint, &title, &traceparent, &tracestate,
		&progressBytes, &errorCode, &errorMessage, &errorRetryable,
		&startedAt, &finishedAt, &nextRetryAt, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return executioncontract.JobView{}, executioncontract.ContractError{
			Code:    "NOT_FOUND",
			Message: "job not found",
		}
	}
	if err != nil {
		return executioncontract.JobView{}, fmt.Errorf("get job: %w", err)
	}

	var progress *executioncontract.Progress
	if len(progressBytes) > 0 {
		var p executioncontract.Progress
		if err := json.Unmarshal(progressBytes, &p); err == nil {
			progress = &p
		}
	}

	var jobErr *executioncontract.JobError
	if errorCode != nil && *errorCode != "" {
		msg := ""
		if errorMessage != nil {
			msg = *errorMessage
		}
		ret := false
		if errorRetryable != nil {
			ret = *errorRetryable
		}
		jobErr = &executioncontract.JobError{
			Code:      *errorCode,
			Message:   msg,
			Retryable: ret,
		}
	}

	view := executioncontract.JobView{
		JobAccepted: executioncontract.JobAccepted{
			JobID:       id,
			ExecutionID: executionID,
			ProposalID:  proposalID,
			DatabaseID:  dbID,
			Status:      executioncontract.JobStatus(status),
			StatusURL:   "/api/databases/" + dbID + "/jobs/" + id,
		},
		MigrationID:       migrationID,
		Revision:          revision,
		Action:            executioncontract.Action(action),
		Mode:              executioncontract.Mode(mode),
		ExecutionStatus:   executioncontract.JobStatus(execStatus),
		AttemptNo:         attemptNo,
		BlocksDatabase:    blocksDatabase,
		Progress:          progress,
		SourceExecutionID: sourceExecID,
		CreatedAt:         createdAt.UTC(),
		StartedAt:         startedAt,
		FinishedAt:        finishedAt,
		NextRetryAt:       nextRetryAt,
		Error:             jobErr,
		AllowedActions:    []executioncontract.AllowedAction{},
	}
	return view, nil
}

type listPosition struct {
	DatabaseID string    `json:"d"`
	Kind       string    `json:"k"`
	Time       time.Time `json:"t"`
	ID         string    `json:"i"`
}

func listCursor(databaseID, kind string, page executioncontract.Page) (int, *listPosition, error) {
	invalid := func() (int, *listPosition, error) {
		return 0, nil, executioncontract.ContractError{Code: "INVALID_REQUEST", Message: "invalid pagination"}
	}
	if _, err := uuid.Parse(databaseID); err != nil {
		return invalid()
	}
	limit := page.Limit
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 {
		return invalid()
	}
	if page.Cursor == nil {
		return limit, nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(*page.Cursor)
	if err != nil {
		return invalid()
	}
	var position listPosition
	if err := json.Unmarshal(raw, &position); err != nil || position.DatabaseID != databaseID || position.Kind != kind || position.Time.IsZero() {
		return invalid()
	}
	id, err := uuid.Parse(position.ID)
	if err != nil || id.String() != position.ID {
		return invalid()
	}
	return limit, &position, nil
}

func encodeListCursor(databaseID, kind string, at time.Time, id string) string {
	raw, _ := json.Marshal(listPosition{DatabaseID: databaseID, Kind: kind, Time: at, ID: id})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (s *Service) ListJobs(ctx context.Context, databaseID string, page executioncontract.Page) (executioncontract.JobPage, error) {
	if s.db == nil {
		return executioncontract.JobPage{}, errors.New("db is required")
	}
	limit, after, err := listCursor(databaseID, "jobs", page)
	if err != nil {
		return executioncontract.JobPage{}, err
	}
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.Time, after.ID
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, created_at FROM app.jobs
		WHERE database_id = $1 AND ($2::timestamptz IS NULL OR (created_at, id) < ($2::timestamptz, $3::uuid))
		ORDER BY created_at DESC, id DESC LIMIT $4`, databaseID, afterTime, afterID, limit+1)
	if err != nil {
		return executioncontract.JobPage{}, fmt.Errorf("list jobs: %w", err)
	}
	type jobPosition struct {
		id string
		at time.Time
	}
	ids := make([]jobPosition, 0, limit+1)
	for rows.Next() {
		var position jobPosition
		if err := rows.Scan(&position.id, &position.at); err != nil {
			rows.Close()
			return executioncontract.JobPage{}, fmt.Errorf("list jobs: %w", err)
		}
		ids = append(ids, position)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return executioncontract.JobPage{}, fmt.Errorf("list jobs: %w", err)
	}
	result := executioncontract.JobPage{Items: make([]executioncontract.JobView, 0, limit)}
	if len(ids) > limit {
		ids = ids[:limit]
		last := ids[len(ids)-1]
		cursor := encodeListCursor(databaseID, "jobs", last.at, last.id)
		result.NextCursor = &cursor
	}
	for _, position := range ids {
		view, err := s.GetJob(ctx, databaseID, position.id)
		if err != nil {
			return executioncontract.JobPage{}, fmt.Errorf("list jobs: %w", err)
		}
		result.Items = append(result.Items, view)
	}
	return result, nil
}

func (s *Service) ListAttempts(ctx context.Context, databaseID string, page executioncontract.Page) (executioncontract.AttemptPage, error) {
	if s.db == nil {
		return executioncontract.AttemptPage{}, errors.New("db is required")
	}
	limit, after, err := listCursor(databaseID, "attempts", page)
	if err != nil {
		return executioncontract.AttemptPage{}, err
	}
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.Time, after.ID
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_id, execution_id, database_id, migration_id, revision,
		       actor_id, attempt_no, status, started_at, finished_at,
		       error_code, error_message, error_retryable, legacy_execution_id
		FROM app.job_attempts
		WHERE database_id = $1 AND ($2::timestamptz IS NULL OR (started_at, id) < ($2::timestamptz, $3::uuid))
		ORDER BY started_at DESC, id DESC LIMIT $4`, databaseID, afterTime, afterID, limit+1)
	if err != nil {
		return executioncontract.AttemptPage{}, fmt.Errorf("list attempts: %w", err)
	}
	defer rows.Close()
	result := executioncontract.AttemptPage{Items: make([]executioncontract.Attempt, 0, limit)}
	for rows.Next() {
		var a executioncontract.Attempt
		var code, message *string
		var retryable *bool
		if err := rows.Scan(&a.AttemptID, &a.JobID, &a.ExecutionID, &a.DatabaseID,
			&a.MigrationID, &a.Revision, &a.ActorID, &a.AttemptNo, &a.Status,
			&a.StartedAt, &a.FinishedAt, &code, &message, &retryable, &a.LegacyExecutionID); err != nil {
			return executioncontract.AttemptPage{}, fmt.Errorf("list attempts: %w", err)
		}
		if len(result.Items) == limit {
			last := result.Items[len(result.Items)-1]
			cursor := encodeListCursor(databaseID, "attempts", last.StartedAt, last.AttemptID)
			result.NextCursor = &cursor
			break
		}
		a.StartedAt = a.StartedAt.UTC()
		if code != nil && *code != "" {
			a.Error = &executioncontract.JobError{Code: *code}
			if message != nil {
				a.Error.Message = *message
			}
			if retryable != nil {
				a.Error.Retryable = *retryable
			}
		}
		result.Items = append(result.Items, a)
	}
	if err := rows.Err(); err != nil {
		return executioncontract.AttemptPage{}, fmt.Errorf("list attempts: %w", err)
	}
	return result, nil
}

func (s *Service) FinalizeJobTx(
	ctx context.Context,
	tx *sql.Tx,
	in executioncontract.JobUpdate,
) error {
	// JobUpdate does not carry a lease token or a receipt-backed recovery identity.
	return errors.New("job finalization requires fenced authority")
}
