package workflow

import (
	"context"
	"crypto/sha256"
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
	Approve(context.Context, uuid.UUID, uuid.UUID) (Proposal, error)
	Get(context.Context, uuid.UUID) (Proposal, error)
}

type Service struct {
	repository serviceRepository
	authorizer authz.Authorizer
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
	validation := validateSnapshot(snapshot)
	validationJSON, _ := json.Marshal(validation)
	id, err := s.repository.CreateProposal(ctx, databaseID, migrationID, actor, snapshot.Revision,
		in.Action, sourceExecutionID, snapshot, snapshotHash, fingerprint, validationJSON)
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

func (s *Service) Approve(ctx context.Context, actor, id uuid.UUID) (Proposal, error) {
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
	return s.repository.Approve(ctx, id, actor)
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
