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
	ProposalID          string                       `json:"proposal_id"`
	DatabaseID          string                       `json:"database_id"`
	MigrationID         string                       `json:"migration_id"`
	Revision            int                          `json:"revision"`
	Action              string                       `json:"action"`
	SourceExecutionID   *string                      `json:"source_execution_id"`
	Status              string                       `json:"status"`
	Snapshot            Snapshot                     `json:"snapshot"`
	SnapshotHash        string                       `json:"snapshot_hash"`
	ApprovalFingerprint string                       `json:"approval_fingerprint"`
	Validation          executioncontract.Validation `json:"validation"`
	Approvals           []Approval                   `json:"approvals"`
}

type Approval struct {
	GroupID      string     `json:"group_id"`
	LeaderUserID *string    `json:"leader_user_id"`
	Status       string     `json:"status"`
	ApprovedAt   *time.Time `json:"approved_at"`
}

type CreateInput struct {
	DatabaseID        string  `json:"database_id"`
	MigrationID       string  `json:"migration_id"`
	Action            string  `json:"action"`
	SourceExecutionID *string `json:"source_execution_id"`
}

type serviceRepository interface {
	LoadDraft(context.Context, uuid.UUID, uuid.UUID) (Snapshot, error)
	FingerprintForDatabase(context.Context, uuid.UUID) ([]uuid.UUID, [32]byte, error)
	CreateProposal(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int, string, *uuid.UUID, Snapshot, [32]byte, [32]byte, []byte) (uuid.UUID, error)
	Submit(context.Context, uuid.UUID, uuid.UUID) (Proposal, error)
	Approve(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int) (Proposal, error)
	Get(context.Context, uuid.UUID) (Proposal, error)
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
	if err = s.authorizer.RequireAccess(ctx, actor, databaseID, authz.AccessEdit); err != nil {
		return Proposal{}, err
	}
	snapshot, err := s.repository.LoadDraft(ctx, databaseID, migrationID)
	if err != nil {
		return Proposal{}, err
	}
	var sourceExecutionID *uuid.UUID
	if in.SourceExecutionID != nil {
		parsed, err := uuid.Parse(*in.SourceExecutionID)
		if err != nil {
			return Proposal{}, ErrInvalidProposal
		}
		sourceExecutionID = &parsed
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

func (s *Service) Submit(ctx context.Context, actor, id uuid.UUID) (Proposal, error) {
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
	return s.repository.Submit(ctx, id, actor)
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

type EnqueueInput struct {
	RequestID         string  `json:"request_id"`
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

	var status, action string
	var revision int
	var targetID sql.NullString
	var snapshotJSON []byte
	var snapshotHash, approvalFingerprint []byte
	err = tx.QueryRowContext(ctx, `SELECT p.status,p.action,p.revision,p.snapshot,p.snapshot_hash,p.approval_fingerprint,
		c.execution_target_id::text
		FROM app.migration_proposals p
		JOIN app.database_connections c ON c.id=p.database_id
		WHERE p.id=$1 AND p.database_id=$2 AND p.migration_id=$3
		FOR UPDATE OF p`, proposalID, databaseID, migrationID).
		Scan(&status, &action, &revision, &snapshotJSON, &snapshotHash, &approvalFingerprint, &targetID)
	if err == sql.ErrNoRows {
		return EnqueueOutput{}, ErrNotFound
	}
	if err != nil {
		return EnqueueOutput{}, err
	}
	if status != "approved" && status != "queued" {
		return EnqueueOutput{}, ErrConflict
	}
	if !targetID.Valid {
		return EnqueueOutput{}, executioncontract.ContractError{Code: "TARGET_UNAVAILABLE", Message: "Execution target is not ready", Retryable: true}
	}
	var draftRevision int
	err = tx.QueryRowContext(ctx, `SELECT revision FROM app.migration_drafts
		WHERE id=$1 AND database_id=$2 FOR UPDATE`, migrationID, databaseID).Scan(&draftRevision)
	if err == sql.ErrNoRows || draftRevision != revision {
		return EnqueueOutput{}, ErrProposalStale
	}
	if err != nil {
		return EnqueueOutput{}, err
	}

	var fingerprint []byte
	_ = tx.QueryRowContext(ctx, `SELECT approval_fingerprint
		FROM app.migration_proposals WHERE id=$1`, proposalID).Scan(&fingerprint)
	currentFingerprint, err := approvalFingerprintTx(ctx, tx, databaseID)
	if err != nil {
		return EnqueueOutput{}, err
	}
	if len(fingerprint) != 32 || !equalBytes(fingerprint, currentFingerprint[:]) {
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
	err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT gr.group_id),
		COUNT(DISTINCT gr.group_id) FILTER (WHERE EXISTS (
			SELECT 1 FROM app.migration_proposal_approvals a
			WHERE a.proposal_id=$1 AND a.group_id=gr.group_id
			  AND a.status='approved'
			  AND a.approval_round=(SELECT MAX(a2.approval_round)
				FROM app.migration_proposal_approvals a2
				WHERE a2.proposal_id=$1 AND a2.group_id=gr.group_id)
		))
		FROM app.group_database_grants gr
		WHERE gr.database_id = $2
			AND gr.access IN ('view', 'edit')`, proposalID, databaseID).Scan(&required, &approved)
	if err != nil {
		return EnqueueOutput{}, err
	}
	if required == 0 || approved != required {
		return EnqueueOutput{}, ErrPendingApprovals
	}

	snapshotHashHex := shaHex(snapshotHash)
	approvalFingerprintHex := shaHex(fingerprint)
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
		value := parsed.String()
		sourceExecutionID = &value
	}
	var snapshot executioncontract.Draft
	if err := json.Unmarshal(snapshotJSON, &snapshot); err != nil {
		return EnqueueOutput{}, ErrInvalidProposal
	}

	result, err := s.jobs.EnqueueTx(ctx, tx, executioncontract.EnqueueRequest{
		TargetID: targetID.String, RequestID: requestID.String(), ActorID: actor.String(),
		DatabaseID: databaseID.String(), ProposalID: proposalID.String(), MigrationID: migrationID.String(),
		SourceExecutionID: sourceExecutionID, Revision: revision, Action: executioncontract.Action(action),
		Direction: direction, Snapshot: snapshot, SnapshotHash: snapshotHashHex,
		ApprovalFingerprint: approvalFingerprintHex,
	})
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
	rows, err := tx.QueryContext(ctx, `SELECT g.id,g.leader_user_id,m.user_id
		FROM app.groups g
		JOIN app.group_database_grants gr ON gr.group_id=g.id
		LEFT JOIN app.group_memberships m ON m.group_id=g.id
		WHERE gr.database_id=$1
		ORDER BY g.id,m.user_id`, databaseID)
	if err != nil {
		return [32]byte{}, err
	}
	defer rows.Close()
	type roster struct {
		Group   string   `json:"group"`
		Leader  *string  `json:"leader"`
		Members []string `json:"members"`
	}
	var all []roster
	var current *roster
	for rows.Next() {
		var group uuid.UUID
		var leader, member sql.NullString
		if err := rows.Scan(&group, &leader, &member); err != nil {
			return [32]byte{}, err
		}
		if current == nil || current.Group != group.String() {
			all = append(all, roster{Group: group.String()})
			current = &all[len(all)-1]
		}
		if leader.Valid {
			value := leader.String
			current.Leader = &value
		}
		if member.Valid {
			current.Members = append(current.Members, member.String)
		}
	}
	if err := rows.Err(); err != nil {
		return [32]byte{}, err
	}
	raw, err := json.Marshal(all)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
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
