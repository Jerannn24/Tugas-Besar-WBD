package workflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/modules/authz"
	"github.com/Jerannn24/Tugas-Besar-WBD.git/telemetry"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidProposal = errors.New("invalid proposal")
	ErrProposalStale   = errors.New("proposal is stale")
	ErrNotFound        = errors.New("proposal not found")
	ErrConflict        = errors.New("proposal state conflict")
)

type roster struct {
	Group   string   `json:"group"`
	Leader  *string  `json:"leader"`
	Members []string `json:"members"`
}
type Snapshot struct {
	MigrationID        string         `json:"migration_id"`
	DatabaseID         string         `json:"database_id"`
	Title              string         `json:"title"`
	SQLUp              string         `json:"sql_up"`
	SQLDown            *string        `json:"sql_down"`
	IrreversibleReason *string        `json:"irreversible_reason"`
	Revision           int            `json:"revision"`
	Mode               string         `json:"mode"`
	Batch              map[string]any `json:"batch"`
}
type snapshot = Snapshot

type Proposal struct {
	ProposalID          string                        `json:"proposal_id"`
	DatabaseID          string                        `json:"database_id"`
	MigrationID         string                        `json:"migration_id"`
	Revision            int                           `json:"revision"`
	Action              string                        `json:"action"`
	SourceExecutionID   *string                       `json:"source_execution_id"`
	Status              string                        `json:"status"`
	Snapshot            Snapshot                      `json:"snapshot"`
	SnapshotHash        string                        `json:"-"`
	ApprovalFingerprint string                        `json:"-"`
	Validation          *executioncontract.Validation `json:"validation"`
	Approvals           []Approval                    `json:"approvals"`
	AllowedActions      []string                      `json:"allowed_actions"`
}

type Approval struct {
	GroupID      string     `json:"group_id"`
	LeaderUserID *string    `json:"leader_user_id"`
	Status       string     `json:"status"`
	ApprovedAt   *time.Time `json:"approved_at"`
}

type CreateInput struct {
	DatabaseID        string  `json:"database_id"`
	Revision          int     `json:"revision"`
	MigrationID       string  `json:"migration_id"`
	Action            string  `json:"action"`
	SourceExecutionID *string `json:"source_execution_id"`
}

type serviceRepository interface {
	LoadDraft(context.Context, uuid.UUID, uuid.UUID) (Snapshot, error)
	sourceSnapshot(context.Context, uuid.UUID, uuid.UUID, int, string, *uuid.UUID, string) (Snapshot, error)
	FingerprintForDatabase(context.Context, uuid.UUID) ([]uuid.UUID, [32]byte, error)
	CreateProposal(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int, string, *uuid.UUID, Snapshot, [32]byte, [32]byte, []byte) (uuid.UUID, error)
	Submit(context.Context, uuid.UUID, uuid.UUID, int) (Proposal, error)
	Approve(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int) (Proposal, error)
	Get(context.Context, uuid.UUID) (Proposal, error)
	ListIDs(context.Context, uuid.UUID, *uuid.UUID, int) ([]uuid.UUID, error)
	MayReview(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error)
	mayEnqueue(context.Context, uuid.UUID, uuid.UUID) error
}

type Service struct {
	repository serviceRepository
	authorizer authz.Authorizer
	jobs       executioncontract.Jobs
	platform   *sql.DB
}

func NewService(dependencies any, authorizer authz.Authorizer) (*Service, error) {
	if authorizer == nil {
		return nil, errors.New("workflow dependencies are required")
	}
	var repository serviceRepository
	switch value := dependencies.(type) {
	case *Repository:
		repository = value
	case *pgxpool.Pool:
		if value == nil {
			return nil, errors.New("workflow dependencies are required")
		}
		repository = NewRepository(value)
	default:
		return nil, errors.New("workflow dependencies are required")
	}
	return &Service{repository: repository, authorizer: authorizer}, nil
}

func (s *Service) Create(ctx context.Context, actor uuid.UUID, in CreateInput) (Proposal, error) {
	databaseID, err := uuid.Parse(in.DatabaseID)
	if err != nil {
		return Proposal{}, ErrInvalidProposal
	}
	migrationID, err := uuid.Parse(in.MigrationID)
	if err != nil || !validAction(in.Action) {
		return Proposal{}, ErrInvalidProposal
	}
	if in.Action != "apply" && in.SourceExecutionID == nil || in.Action == "apply" && in.SourceExecutionID != nil {
		return Proposal{}, ErrInvalidProposal
	}
	if err = s.authorizer.RequireAccess(ctx, actor, databaseID, authz.AccessEdit); err != nil {
		return Proposal{}, err
	}
	snapshot, err := s.repository.LoadDraft(ctx, databaseID, migrationID)
	if err != nil {
		return Proposal{}, err
	}
	if in.Revision > 0 && in.Revision != snapshot.Revision {
		return Proposal{}, ErrProposalStale
	}
	var sourceExecutionID *uuid.UUID
	if in.SourceExecutionID != nil {
		parsed, err := uuid.Parse(*in.SourceExecutionID)
		if err != nil {
			return Proposal{}, ErrInvalidProposal
		}
		sourceExecutionID = &parsed
	}
	if sourceExecutionID != nil {
		snapshot, err = s.repository.sourceSnapshot(ctx, databaseID, migrationID, snapshot.Revision, in.Action, sourceExecutionID, snapshot.Mode)
		if err != nil {
			return Proposal{}, err
		}
	}
	snapshotHash, err := digest(snapshot)
	if err != nil {
		return Proposal{}, err
	}
	groups, fingerprint, err := s.repository.FingerprintForDatabase(ctx, databaseID)
	if err != nil {
		return Proposal{}, err
	}
	id, err := s.repository.CreateProposal(
		ctx,
		databaseID,
		migrationID,
		actor,
		snapshot.Revision,
		in.Action,
		sourceExecutionID,
		snapshot,
		snapshotHash,
		fingerprint,
		nil,
	)
	if err != nil {
		return Proposal{}, err
	}
	proposal, err := s.repository.Get(ctx, id)
	if err != nil {
		return Proposal{}, err
	}
	if len(proposal.Approvals) == 0 {
		for _, groupID := range groups {
			proposal.Approvals = append(proposal.Approvals, Approval{GroupID: groupID.String(), Status: "pending"})
		}
	}
	return proposal, nil
}

func (s *Service) Submit(ctx context.Context, actor, id uuid.UUID, revision int) (Proposal, error) {
	proposal, err := s.repository.Get(ctx, id)
	if err != nil {
		return Proposal{}, err
	}
	databaseID, err := uuid.Parse(proposal.DatabaseID)
	if err != nil {
		return Proposal{}, ErrInvalidProposal
	}
	if err = s.authorizer.RequireAccess(ctx, actor, databaseID, authz.AccessEdit); err != nil {
		return Proposal{}, err
	}
	return s.repository.Submit(ctx, id, actor, revision)
}

func (s *Service) Approve(
	ctx context.Context,
	actor, id, groupID uuid.UUID,
	revision int,
) (Proposal, error) {
	proposal, err := s.repository.Get(ctx, id)
	if err != nil {
		return Proposal{}, err
	}

	databaseID, err := uuid.Parse(proposal.DatabaseID)
	if err != nil {
		return Proposal{}, ErrInvalidProposal
	}

	if err = s.authorizer.RequireAccess(
		ctx,
		actor,
		databaseID,
		authz.AccessView,
	); err != nil {
		return Proposal{}, err
	}

	return s.repository.Approve(ctx, id, actor, groupID, revision)
}

func (s *Service) Get(ctx context.Context, actor, id uuid.UUID) (Proposal, error) {
	proposal, err := s.repository.Get(ctx, id)
	if err != nil {
		return Proposal{}, err
	}
	databaseID, err := uuid.Parse(proposal.DatabaseID)
	if err != nil {
		return Proposal{}, ErrInvalidProposal
	}
	if err = s.authorizer.RequireAccess(ctx, actor, databaseID, authz.AccessView); err != nil {
		return Proposal{}, err
	}
	return proposal, nil
}
func (s *Service) present(ctx context.Context, actor uuid.UUID, proposal Proposal) (Proposal, error) {
	proposal.Approvals = append([]Approval{}, proposal.Approvals...)
	proposal.AllowedActions = []string{}
	databaseID, err := uuid.Parse(proposal.DatabaseID)
	if err != nil {
		return Proposal{}, ErrInvalidProposal
	}
	switch proposal.Status {
	case "draft", "approved":
		err = s.authorizer.RequireAccess(ctx, actor, databaseID, authz.AccessEdit)
		if err == nil && proposal.Status == "draft" {
			proposal.AllowedActions = append(proposal.AllowedActions, "submit_review")
		} else if err == nil {
			id, parseErr := uuid.Parse(proposal.ProposalID)
			if parseErr != nil {
				return Proposal{}, ErrInvalidProposal
			}
			err = s.repository.mayEnqueue(ctx, actor, id)
			if err == nil {
				proposal.AllowedActions = append(proposal.AllowedActions, "enqueue")
			} else if !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrPendingApprovals) {
				return Proposal{}, err
			}
		} else if !errors.Is(err, authz.ErrForbidden) {
			return Proposal{}, err
		}
	case "pending_review":
		id, err := uuid.Parse(proposal.ProposalID)
		if err != nil {
			return Proposal{}, ErrInvalidProposal
		}
		allowed, err := s.repository.MayReview(ctx, actor, databaseID, id)
		if err != nil {
			return Proposal{}, err
		}
		if allowed {
			proposal.AllowedActions = append(proposal.AllowedActions, "approve")
		}
	}
	return proposal, nil
}

type EnqueueInput struct {
	RequestID         string  `json:"request_id"`
	Revision          int     `json:"revision"`
	SourceExecutionID *string `json:"source_execution_id,omitempty"`
}

type EnqueueOutput struct {
	executioncontract.JobAccepted
	Replayed bool `json:"replayed"`
}

func (s *Service) SetJobs(jobs executioncontract.Jobs) {
	s.jobs = jobs
}

func (s *Service) SetPlatform(db *sql.DB) {
	s.platform = db
}

func (s *Service) Enqueue(ctx context.Context, actor, proposalID uuid.UUID, input EnqueueInput) (EnqueueOutput, error) {
	if s.jobs == nil {
		return EnqueueOutput{}, executioncontract.ContractError{Code: "NOT_CONFIGURED", Message: "Job queue is not configured", Retryable: false}
	}
	if s.platform == nil {
		return EnqueueOutput{}, executioncontract.ContractError{Code: "INTERNAL_ERROR", Message: "Platform database is not configured", Retryable: true}
	}
	requestID := uuid.Nil
	parsedRequest, err := uuid.Parse(input.RequestID)
	if err != nil {
		return EnqueueOutput{}, ErrInvalidProposal
	}
	requestID = parsedRequest

	proposal, err := s.repository.Get(ctx, proposalID)
	if err != nil {
		return EnqueueOutput{}, err
	}
	databaseID, err := uuid.Parse(proposal.DatabaseID)
	if err != nil {
		return EnqueueOutput{}, ErrInvalidProposal
	}
	migrationID, err := uuid.Parse(proposal.MigrationID)
	if err != nil {
		return EnqueueOutput{}, ErrInvalidProposal
	}
	if err = s.authorizer.RequireAccess(ctx, actor, databaseID, authz.AccessEdit); err != nil {
		return EnqueueOutput{}, err
	}

	tx, err := s.platform.BeginTx(ctx, nil)
	if err != nil {
		return EnqueueOutput{}, err
	}
	defer tx.Rollback()

	// Lock the connection first, as draft edits and target reservations do.
	var targetID sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT execution_target_id::text FROM app.database_connections
		WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, databaseID).Scan(&targetID)
	if errors.Is(err, sql.ErrNoRows) {
		return EnqueueOutput{}, ErrNotFound
	}
	if err != nil {
		return EnqueueOutput{}, err
	}
	if !targetID.Valid {
		return EnqueueOutput{}, executioncontract.ContractError{Code: "TARGET_UNAVAILABLE", Message: "Execution target is not ready", Retryable: true}
	}

	var status, action string
	var revision int
	var proposalSource sql.NullString
	var snapshotJSON []byte
	var snapshotHash, approvalFingerprint []byte
	err = tx.QueryRowContext(ctx, `SELECT p.status,p.action,p.revision,p.snapshot,p.snapshot_hash,p.approval_fingerprint,
		p.source_execution_id::text
		FROM app.migration_proposals p
		WHERE p.id=$1 AND p.database_id=$2 AND p.migration_id=$3
		FOR UPDATE OF p`, proposalID, databaseID, migrationID).
		Scan(&status, &action, &revision, &snapshotJSON, &snapshotHash, &approvalFingerprint, &proposalSource)
	if err == sql.ErrNoRows {
		return EnqueueOutput{}, ErrNotFound
	}
	if err != nil {
		return EnqueueOutput{}, err
	}
	var originalFingerprint sql.NullString
	var originalError sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT approval_fingerprint,error_code FROM app.jobs
		WHERE database_id=$1 AND request_id=$2`, databaseID, requestID.String()).Scan(&originalFingerprint, &originalError)
	replayRequest := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return EnqueueOutput{}, err
	}

	snapshotHashHex := shaHex(snapshotHash)
	approvalFingerprintHex := shaHex(approvalFingerprint)
	direction := executioncontract.Up
	if action == string(executioncontract.Rollback) || action == string(executioncontract.Compensate) {
		direction = executioncontract.Down
	}
	var sourceExecutionID *executioncontract.ID
	if input.SourceExecutionID != nil {
		parsed, parseErr := uuid.Parse(*input.SourceExecutionID)
		if parseErr != nil {
			return EnqueueOutput{}, ErrInvalidProposal
		}
		if !proposalSource.Valid || parsed.String() != proposalSource.String {
			// Existing request IDs are checked against their original source below.
			if !replayRequest {
				return EnqueueOutput{}, ErrInvalidProposal
			}
			source := parsed.String()
			sourceExecutionID = &source
		}
	}
	if proposalSource.Valid && sourceExecutionID == nil {
		sourceExecutionID = &proposalSource.String
	}
	if action == string(executioncontract.Resume) {
		if !proposalSource.Valid {
			return EnqueueOutput{}, ErrInvalidProposal
		}
		var sourceDirection string
		err = tx.QueryRowContext(ctx, `SELECT direction FROM app.jobs WHERE execution_id=$1 AND database_id=$2
			AND target_id=$3 AND migration_id=$4 AND revision=$5 AND mode='batch'
			ORDER BY created_at,id LIMIT 1`,
			proposalSource.String, databaseID, targetID.String, migrationID, revision).Scan(&sourceDirection)
		if errors.Is(err, sql.ErrNoRows) {
			return EnqueueOutput{}, ErrInvalidProposal
		}
		if err != nil {
			return EnqueueOutput{}, err
		}
		direction = executioncontract.Direction(sourceDirection)
	}
	var snapshot executioncontract.Draft
	if err := json.Unmarshal(snapshotJSON, &snapshot); err != nil {
		return EnqueueOutput{}, ErrInvalidProposal
	}

	var traceparent, tracestate string
	if trace, ok := telemetry.TraceFromContext(ctx); ok {
		traceparent, tracestate = trace.Traceparent(), trace.TraceState
	}
	requestRevision := revision
	if replayRequest && input.Revision > 0 {
		requestRevision = input.Revision
	}
	if replayRequest && status == "pending_review" && originalError.Valid && originalError.String == "REVIEW_STALE" {
		approvalFingerprintHex = originalFingerprint.String
	}
	request := executioncontract.EnqueueRequest{
		TargetID: targetID.String, RequestID: requestID.String(), ActorID: actor.String(),
		DatabaseID: databaseID.String(), ProposalID: proposalID.String(), MigrationID: migrationID.String(),
		SourceExecutionID: sourceExecutionID, Revision: requestRevision, Action: executioncontract.Action(action),
		Direction: direction, Snapshot: snapshot, SnapshotHash: snapshotHashHex,
		ApprovalFingerprint: approvalFingerprintHex, Traceparent: traceparent, Tracestate: tracestate,
	}
	if replayRequest {
		result, err := s.jobs.EnqueueTx(ctx, tx, request)
		if err != nil {
			return EnqueueOutput{}, err
		}
		return EnqueueOutput{JobAccepted: result.JobAccepted, Replayed: result.Replayed}, nil
	}

	if input.Revision > 0 && input.Revision != revision {
		return EnqueueOutput{}, ErrProposalStale
	}
	if status == "pending_review" {
		return EnqueueOutput{}, ErrPendingApprovals
	}
	if status != "approved" && status != "queued" {
		return EnqueueOutput{}, ErrConflict
	}
	var draftRevision int
	err = tx.QueryRowContext(ctx, `SELECT revision FROM app.migration_drafts
		WHERE id=$1 AND database_id=$2 FOR UPDATE`, migrationID, databaseID).Scan(&draftRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return EnqueueOutput{}, ErrProposalStale
	}
	if err != nil {
		return EnqueueOutput{}, err
	}
	if draftRevision != revision {
		return EnqueueOutput{}, ErrProposalStale
	}

	currentFingerprint, err := approvalFingerprintTx(ctx, tx, databaseID)
	if err != nil {
		return EnqueueOutput{}, err
	}
	if !equalBytes(approvalFingerprint, currentFingerprint[:]) {
		if status == "queued" {
			// A worker must fence the old job before reopening a queued proposal.
			return EnqueueOutput{}, ErrProposalStale
		}
		_, err = tx.ExecContext(
			ctx,
			`UPDATE app.migration_proposals
			SET status = 'superseded'
			WHERE id = $1`,
			proposalID,
		)
		if err != nil {
			return EnqueueOutput{}, err
		}

		if err = tx.Commit(); err != nil {
			return EnqueueOutput{}, err
		}

		return EnqueueOutput{}, ErrProposalStale
	}
	var required, approved int
	err = tx.QueryRowContext(ctx, currentApprovalCountsQuery, proposalID, databaseID, nil).Scan(&required, &approved)
	if err != nil {
		return EnqueueOutput{}, err
	}
	if required == 0 || approved != required {
		return EnqueueOutput{}, ErrPendingApprovals
	}

	result, err := s.jobs.EnqueueTx(ctx, tx, request)
	if err != nil {
		return EnqueueOutput{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE app.migration_proposals
		SET status='queued' WHERE id=$1 AND status IN ('approved','queued')`, proposalID); err != nil {
		return EnqueueOutput{}, err
	}
	if err = tx.Commit(); err != nil {
		return EnqueueOutput{}, err
	}
	return EnqueueOutput{JobAccepted: result.JobAccepted, Replayed: result.Replayed}, nil
}

func shaHex(value []byte) string {
	return hex.EncodeToString(value)
}

func approvalFingerprintTx(ctx context.Context, tx *sql.Tx, databaseID uuid.UUID) ([32]byte, error) {
	rows, err := tx.QueryContext(ctx, approvalRosterQuery, databaseID)
	if err != nil {
		return [32]byte{}, err
	}
	defer rows.Close()
	_, fingerprint, err := scanFingerprintRows(rows)
	return fingerprint, err
}

func validateSnapshot(snapshot Snapshot) executioncontract.Validation {
	issues := []executioncontract.Issue{}
	if strings.TrimSpace(snapshot.Title) == "" {
		issues = append(issues, executioncontract.Issue{Code: "INVALID_TITLE", Message: "title is required"})
	}
	if strings.TrimSpace(snapshot.SQLUp) == "" {
		issues = append(issues, executioncontract.Issue{Code: "INVALID_SQL", Message: "sql is required"})
	}
	status := "valid"
	if len(issues) > 0 {
		status = "invalid"
	}
	return executioncontract.Validation{Status: status, CheckedAt: time.Now().UTC(), Issues: issues}
}

func digest(value any) ([32]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}

func validAction(value string) bool {
	switch value {
	case "apply", "rollback", "forward", "resume", "compensate":
		return true
	default:
		return false
	}
}
