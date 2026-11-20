package workflow

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Access string

const (
	AccessNone Access = "none"
	AccessView Access = "view"
	AccessEdit Access = "edit"
)

var (
	ErrForbidden        = errors.New("workflow action is forbidden")
	ErrPendingApprovals = errors.New("workflow approvals are incomplete")
)

type ApprovalRequirement struct {
	GroupID      uuid.UUID
	LeaderUserID *uuid.UUID
	Approved     bool
}

type ApprovalSummary struct {
	Required int
	Approved int
}

func (s ApprovalSummary) Complete() bool {
	return s.Required > 0 && s.Required == s.Approved
}

func EffectiveAccess(accesses ...Access) Access {
	result := AccessNone
	for _, access := range accesses {
		if access == AccessEdit {
			return AccessEdit
		}
		if access == AccessView {
			result = AccessView
		}
	}
	return result
}

func MayApprove(actorID uuid.UUID, requirement ApprovalRequirement) bool {
	return actorID != uuid.Nil &&
		requirement.LeaderUserID != nil &&
		*requirement.LeaderUserID == actorID &&
		!requirement.Approved
}

type queryer interface {
	QueryjadRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type Policy struct {
	db queryer
}

func NewPolicy(db *pgxpool.Pool) *Policy {
	return &Policy{db: db}
}

func (p *Policy) EffectiveAccess(ctx context.Context, actorID, databaseID uuid.UUID) (Access, error) {
	if actorID == uuid.Nil || databaseID == uuid.Nil {
		return AccessNone, ErrForbidden
	}
	var rank int
	err := p.db.QueryRow(ctx, `
		SELECT COALESCE(MAX(CASE g.access WHEN 'edit' THEN 2 WHEN 'view' THEN 1 ELSE 0 END), 0)
		FROM app.group_memberships m
		JOIN app.users u ON u.id = m.user_id AND u.is_active
		JOIN app.group_database_grants g ON g.group_id = m.group_id
		JOIN app.database_connections d ON d.id = g.database_id AND d.deleted_at IS NULL
		WHERE m.user_id=$1 AND d.id=$2`, actorID, databaseID).Scan(&rank)
	if err != nil {
		return AccessNone, err
	}
	switch rank {
	case 2:
		return AccessEdit, nil
	case 1:
		return AccessView, nil
	default:
		return AccessNone, nil
	}
}

func (p *Policy) MayEnqueue(ctx context.Context, actorID, proposalID uuid.UUID) error {
	var databaseID uuid.UUID
	if err := p.db.QueryRow(ctx, `
		SELECT database_id FROM app.migration_proposals
		WHERE id=$1 AND status IN ('approved','queued')`, proposalID).Scan(&databaseID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrForbidden
		}
		return err
	}
	access, err := p.EffectiveAccess(ctx, actorID, databaseID)
	if err != nil {
		return err
	}
	if access != AccessEdit {
		return ErrForbidden
	}
	summary, err := p.ApprovalSummary(ctx, proposalID)
	if err != nil {
		return err
	}
	if !summary.Complete() {
		return ErrPendingApprovals
	}
	return nil
}

func (p *Policy) MayApprove(ctx context.Context, actorID, proposalID uuid.UUID) (bool, error) {
	if actorID == uuid.Nil || proposalID == uuid.Nil {
		return false, ErrForbidden
	}
	var allowed bool
	err := p.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM app.migration_proposals p
			JOIN app.group_database_grants grant_row ON grant_row.database_id = p.database_id
			JOIN app.groups g ON g.id = grant_row.group_id
			JOIN app.users u ON u.id = g.leader_user_id AND u.is_active
			WHERE p.id=$1 AND p.status='pending_review' AND g.leader_user_id=$2
		)`, proposalID, actorID).Scan(&allowed)
	return allowed, err
}

func (p *Policy) ApprovalSummary(ctx context.Context, proposalID uuid.UUID) (ApprovalSummary, error) {
	rows, err := p.db.Query(ctx, `
		SELECT g.id, g.leader_user_id,
		       COALESCE((
		           SELECT g.leader_user_id IS NOT NULL
		               AND u.is_active
		               AND a.status='approved'
		               AND a.leader_user_id=g.leader_user_id
		           FROM app.migration_proposal_approvals a
		           WHERE a.proposal_id=$1 AND a.group_id=g.id
		           ORDER BY a.approval_round DESC
		           LIMIT 1
		       ), false)
		FROM app.migration_proposals p
		JOIN app.group_database_grants grant_row ON grant_row.database_id=p.database_id
		JOIN app.groups g ON g.id=grant_row.group_id
		LEFT JOIN app.users u ON u.id=g.leader_user_id
		WHERE p.id=$1`, proposalID)
	if err != nil {
		return ApprovalSummary{}, err
	}
	defer rows.Close()
	var summary ApprovalSummary
	for rows.Next() {
		var requirement ApprovalRequirement
		if err := rows.Scan(&requirement.GroupID, &requirement.LeaderUserID, &requirement.Approved); err != nil {
			return ApprovalSummary{}, err
		}
		summary.Required++
		if requirement.Approved && requirement.LeaderUserID != nil {
			summary.Approved++
		}
	}
	if err := rows.Err(); err != nil {
		return ApprovalSummary{}, err
	}
	return summary, nil
}
