package jobs

import (
	"context"
	"database/sql"
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
	if in.Action == executioncontract.Resume {
		if in.SourceExecutionID == nil || *in.SourceExecutionID == "" {
			return executioncontract.EnqueueResult{}, makeRequestConflict("resume requires source execution id")
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

func (s *Service) ListJobs(
	ctx context.Context,
	databaseID string,
	page executioncontract.Page,
) (executioncontract.JobPage, error) {
	// shortcut: stubbed for first P1 increment, implement pagination in subsequent P1 increment
	return executioncontract.JobPage{Items: []executioncontract.JobView{}}, nil
}

func (s *Service) ListAttempts(
	ctx context.Context,
	databaseID string,
	page executioncontract.Page,
) (executioncontract.AttemptPage, error) {
	// shortcut: stubbed for first P1 increment, implement attempt listing in subsequent P1 increment
	return executioncontract.AttemptPage{Items: []executioncontract.Attempt{}}, nil
}

func (s *Service) FinalizeJobTx(
	ctx context.Context,
	tx *sql.Tx,
	in executioncontract.JobUpdate,
) error {
	// shortcut: stubbed for first P1 increment, implement job finalization in subsequent increment
	return nil
}
