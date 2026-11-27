package executioncontract

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

type ID = string
type ProposalStatus string
type JobStatus string
type Action string
type AllowedAction string
type Direction string
type Mode string

const (
	DraftStatus    ProposalStatus = "draft"
	PendingReview  ProposalStatus = "pending_review"
	Approved       ProposalStatus = "approved"
	ProposalQueued ProposalStatus = "queued"
	Superseded     ProposalStatus = "superseded"
)

const (
	Queued            JobStatus = "queued"
	Running           JobStatus = "running"
	RetryWait         JobStatus = "retry_wait"
	Succeeded         JobStatus = "succeeded"
	Failed            JobStatus = "failed"
	Partial           JobStatus = "partial"
	PartiallyReverted JobStatus = "partially_reverted"
	Unknown           JobStatus = "unknown"
)

const (
	Apply      Action = "apply"
	Rollback   Action = "rollback"
	Forward    Action = "forward"
	Resume     Action = "resume"
	Compensate Action = "compensate"

	Up   Direction = "up"
	Down Direction = "down"

	Standard Mode = "standard"
	Batch    Mode = "batch"
)

type BatchConfig struct {
	Schema       string         `json:"schema"`
	Table        string         `json:"table"`
	CursorColumn string         `json:"cursor_column"`
	BatchSize    int            `json:"batch_size"`
	Parameters   map[string]any `json:"parameters"`
}

type Draft struct {
	MigrationID        ID           `json:"migration_id"`
	DatabaseID         ID           `json:"database_id"`
	Title              string       `json:"title"`
	SQLUp              string       `json:"sql_up"`
	SQLDown            *string      `json:"sql_down"`
	IrreversibleReason *string      `json:"irreversible_reason"`
	Revision           int          `json:"revision"`
	Mode               Mode         `json:"mode"`
	Batch              *BatchConfig `json:"batch"`
}

func ValidateDraft(d Draft) error {
	title := strings.TrimSpace(d.Title)

	if title != d.Title ||
		utf8.RuneCountInString(title) < 1 ||
		utf8.RuneCountInString(title) > 100 {
		return errors.New("invalid title")
	}

	if strings.TrimSpace(d.SQLUp) == "" ||
		len(d.SQLUp) > 262144 {
		return errors.New("invalid sql_up")
	}

	if d.SQLDown != nil &&
		(strings.TrimSpace(*d.SQLDown) == "" ||
			len(*d.SQLDown) > 262144) {
		return errors.New("invalid sql_down")
	}

	if d.SQLDown == nil &&
		(d.IrreversibleReason == nil ||
			strings.TrimSpace(*d.IrreversibleReason) == "") {
		return errors.New("irreversible reason required")
	}

	if d.Revision < 1 ||
		(d.Mode != Standard && d.Mode != Batch) {
		return errors.New("invalid revision or mode")
	}

	if d.Mode == Standard && d.Batch != nil ||
		d.Mode == Batch &&
			(d.Batch == nil ||
				d.Batch.BatchSize < 1 ||
				d.Batch.BatchSize > 10000) {
		return errors.New("invalid batch")
	}

	return nil
}

type Issue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Validation struct {
	Status    string    `json:"status"`
	CheckedAt time.Time `json:"checked_at"`
	Issues    []Issue   `json:"issues"`
}

type Approval struct {
	GroupID      ID         `json:"group_id"`
	LeaderUserID ID         `json:"leader_user_id"`
	Status       string     `json:"status"`
	ApprovedAt   *time.Time `json:"approved_at"`
}

type Proposal struct {
	ProposalID        ID              `json:"proposal_id"`
	DatabaseID        ID              `json:"database_id"`
	MigrationID       ID              `json:"migration_id"`
	Revision          int             `json:"revision"`
	Action            Action          `json:"action"`
	SourceExecutionID *ID             `json:"source_execution_id"`
	Status            ProposalStatus  `json:"status"`
	Snapshot          Draft           `json:"snapshot"`
	Validation        Validation      `json:"validation"`
	Approvals         []Approval      `json:"approvals"`
	AllowedActions    []AllowedAction `json:"allowed_actions"`
}

type Progress struct {
	Direction        Direction `json:"direction"`
	BatchesCommitted string    `json:"batches_committed"`
	RowsCommitted    string    `json:"rows_committed"`
	BatchesReverted  string    `json:"batches_reverted"`
	RowsReverted     string    `json:"rows_reverted"`
	TotalRows        *string   `json:"total_rows"`
}

type JobError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type JobAccepted struct {
	JobID       ID        `json:"job_id"`
	ExecutionID ID        `json:"execution_id"`
	ProposalID  ID        `json:"proposal_id"`
	DatabaseID  ID        `json:"database_id"`
	Status      JobStatus `json:"status"`
	StatusURL   string    `json:"status_url"`
}

type EnqueueResult struct {
	JobAccepted
	Replayed bool `json:"-"`
}

type JobView struct {
	JobAccepted
	MigrationID       ID              `json:"migration_id"`
	Revision          int             `json:"revision"`
	Action            Action          `json:"action"`
	Mode              Mode            `json:"mode"`
	ExecutionStatus   JobStatus       `json:"execution_status"`
	AttemptNo         int             `json:"attempt_no"`
	BlocksDatabase    bool            `json:"blocks_database"`
	Progress          *Progress       `json:"progress"`
	SourceExecutionID *ID             `json:"source_execution_id"`
	CreatedAt         time.Time       `json:"created_at"`
	StartedAt         *time.Time      `json:"started_at"`
	FinishedAt        *time.Time      `json:"finished_at"`
	NextRetryAt       *time.Time      `json:"next_retry_at"`
	Error             *JobError       `json:"error"`
	AllowedActions    []AllowedAction `json:"allowed_actions"`
}

type Actor struct {
	ID       ID     `json:"id"`
	Username string `json:"username"`
}

type HistoryError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type HistoryEntry struct {
	ID                 ID            `json:"id"`
	DatabaseID         ID            `json:"database_id"`
	DraftID            ID            `json:"draft_id"`
	DraftRevision      int           `json:"draft_revision"`
	Title              string        `json:"title"`
	SQL                string        `json:"sql"`
	StartedAt          time.Time     `json:"started_at"`
	FinishedAt         *time.Time    `json:"finished_at"`
	Status             string        `json:"status"`
	Error              *HistoryError `json:"error"`
	ExecutionID        ID            `json:"execution_id"`
	MigrationID        ID            `json:"migration_id"`
	Revision           int           `json:"revision"`
	Version            string        `json:"version"`
	Action             Action        `json:"action"`
	Direction          Direction     `json:"direction"`
	ExecutedBy         Actor         `json:"executed_by"`
	CommittedAt        time.Time     `json:"committed_at"`
	SQLSnapshot        string        `json:"sql_snapshot"`
	SourceExecutionID  *ID           `json:"source_execution_id"`
	RevertsExecutionID *ID           `json:"reverts_execution_id"`
	IsComplete         bool          `json:"is_complete"`
	BatchCommits       []BatchCommit `json:"batch_commits"`
}

type BatchCommit struct {
	JobID          ID             `json:"job_id"`
	ExecutedBy     Actor          `json:"executed_by"`
	RangeStart     *string        `json:"range_start"`
	RangeEnd       *string        `json:"range_end"`
	SQLSnapshot    string         `json:"sql_snapshot"`
	SafeParameters map[string]any `json:"safe_parameters"`
	BatchIndex     string         `json:"batch_index"`
	RowCount       string         `json:"row_count"`
	CommittedAt    time.Time      `json:"committed_at"`
	Receipt        string         `json:"receipt"`
}

type EnqueueRequest struct {
	TargetID, RequestID, ActorID, DatabaseID, ProposalID, MigrationID ID
	SourceExecutionID                                                 *ID
	Revision                                                          int
	Action                                                            Action
	Direction                                                         Direction
	Snapshot                                                          Draft
	SnapshotHash, ApprovalFingerprint, Traceparent, Tracestate        string
}

type Page struct {
	Limit  int
	Cursor *string
}

type JobPage struct {
	Items      []JobView `json:"items"`
	NextCursor *string   `json:"next_cursor"`
}

type Attempt struct {
	AttemptID         ID         `json:"attempt_id"`
	JobID             *ID        `json:"job_id"`
	ExecutionID       ID         `json:"execution_id"`
	DatabaseID        ID         `json:"database_id"`
	MigrationID       ID         `json:"migration_id"`
	Revision          int        `json:"revision"`
	ActorID           ID         `json:"actor_id"`
	AttemptNo         int        `json:"attempt_no"`
	Status            JobStatus  `json:"status"`
	StartedAt         time.Time  `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
	Error             *JobError  `json:"error"`
	LegacyExecutionID *ID        `json:"legacy_execution_id"`
}

type AttemptPage struct {
	Items      []Attempt `json:"items"`
	NextCursor *string   `json:"next_cursor"`
}

type RevalidationRequest struct {
	JobID, ExecutionID, ActorID, DatabaseID, TargetID, MigrationID, ProposalID ID
	Revision                                                                   int
	Action                                                                     Action
	Direction                                                                  Direction
	SourceExecutionID                                                          *ID
	SnapshotHash, ApprovalFingerprint                                          string
}

type ExecutionPlan struct {
	Request       RevalidationRequest
	Snapshot      Draft
	Version       string
	ActorUsername string
	ValidatedAt   time.Time
}

type CommitEvidence struct {
	JobID, ExecutionID, ProposalID, MigrationID, ActorID, DatabaseID, TargetID ID
	Revision                                                                   int
	Version                                                                    string
	Action                                                                     Action
	Direction                                                                  Direction
	SQLSnapshot                                                                string
	SafeParameters                                                             map[string]any
	SnapshotHash, Receipt                                                      string
	CommittedAt                                                                time.Time
	RowCount                                                                   string
	SourceExecutionID                                                          *ID
	Batch                                                                      *BatchCommit
	FinalStep                                                                  bool
}

type JobUpdate struct {
	Job JobView
}

type Jobs interface {
	EnqueueTx(
		ctx context.Context,
		tx *sql.Tx,
		in EnqueueRequest,
	) (EnqueueResult, error)

	GetJob(
		ctx context.Context,
		databaseID string,
		jobID string,
	) (JobView, error)

	ListJobs(
		ctx context.Context,
		databaseID string,
		page Page,
	) (JobPage, error)

	ListAttempts(
		ctx context.Context,
		databaseID string,
		page Page,
	) (AttemptPage, error)

	FinalizeJobTx(
		ctx context.Context,
		tx *sql.Tx,
		in JobUpdate,
	) error
}

type WorkflowGate interface {
	Revalidate(
		ctx context.Context,
		in RevalidationRequest,
	) (ExecutionPlan, error)

	AllowedActions(
		ctx context.Context,
		actorID string,
		databaseID string,
		executionID string,
	) ([]AllowedAction, error)

	RecordCommitTx(
		ctx context.Context,
		tx *sql.Tx,
		in CommitEvidence,
	) error
}

type ContractError struct {
	Code        string            `json:"code"`
	Message     string            `json:"message"`
	FieldErrors map[string]string `json:"field_errors"`
	Issues      []Issue           `json:"issues,omitempty"`
	Retryable   bool              `json:"retryable"`
}

func (e ContractError) Error() string {
	return e.Code + ": " + e.Message
}
