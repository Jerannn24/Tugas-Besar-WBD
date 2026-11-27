package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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
	ErrLeaderRequired   = errors.New("current user is not an eligible group leader")
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
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type Policy struct {
	db queryer
}

func NewPolicy(db *pgxpool.Pool) *Policy {
	return &Policy{db: db}
}

// EffectiveAccess menghitung akses tertinggi aktor terhadap database
// berdasarkan membership aktif dan grant dari seluruh grup.
func (p *Policy) EffectiveAccess(
	ctx context.Context,
	actorID, databaseID uuid.UUID,
) (Access, error) {
	if p == nil || p.db == nil ||
		actorID == uuid.Nil || databaseID == uuid.Nil {
		return AccessNone, ErrForbidden
	}

	var rank int

	err := p.db.QueryRow(ctx, `
		SELECT COALESCE(
			MAX(
				CASE grant_row.access
					WHEN 'edit' THEN 2
					WHEN 'view' THEN 1
					ELSE 0
				END
			),
			0
		)
		FROM app.group_memberships membership
		JOIN app.users member_user
			ON member_user.id = membership.user_id
			AND member_user.is_active
		JOIN app.group_database_grants grant_row
			ON grant_row.group_id = membership.group_id
		JOIN app.database_connections database_row
			ON database_row.id = grant_row.database_id
			AND database_row.deleted_at IS NULL
		WHERE membership.user_id = $1
			AND database_row.id = $2
			AND grant_row.access IN ('view', 'edit')
	`, actorID, databaseID).Scan(&rank)

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

// MayEnqueue memeriksa izin aktor, kelengkapan approval,
// dan kesesuaian fingerprint akses terbaru.
func (p *Policy) MayEnqueue(
	ctx context.Context,
	actorID, proposalID uuid.UUID,
) error {
	if p == nil || p.db == nil ||
		actorID == uuid.Nil || proposalID == uuid.Nil {
		return ErrForbidden
	}

	var (
		databaseID        uuid.UUID
		revision          int
		storedFingerprint []byte
	)

	err := p.db.QueryRow(ctx, `
		SELECT
			proposal.database_id,
			proposal.revision,
			proposal.approval_fingerprint
		FROM app.migration_proposals proposal
		JOIN app.migration_drafts draft
			ON draft.id = proposal.migration_id
			AND draft.revision = proposal.revision
		WHERE proposal.id = $1
			AND proposal.status IN ('approved', 'queued')
	`, proposalID).Scan(
		&databaseID,
		&revision,
		&storedFingerprint,
	)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrForbidden
		}
		return err
	}

	_ = revision // Revision sudah diverifikasi oleh query.

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

	currentFingerprint, err := currentApprovalFingerprint(
		ctx,
		p.db,
		databaseID,
	)
	if err != nil {
		return err
	}

	if len(storedFingerprint) != sha256.Size ||
		!bytes.Equal(storedFingerprint, currentFingerprint[:]) {
		return ErrPendingApprovals
	}

	return nil
}

// currentApprovalFingerprint menghitung fingerprint dari kondisi
// grant, leader, dan membership seluruh grup yang memiliki akses
// terhadap database.
func currentApprovalFingerprint(
	ctx context.Context,
	db queryer,
	databaseID uuid.UUID,
) ([32]byte, error) {
	rows, err := db.Query(ctx, `
		SELECT
			group_row.id,
			group_row.leader_user_id,
			COALESCE(leader_user.is_active, FALSE),
			grant_row.access,
			membership.user_id,
			COALESCE(member_user.is_active, FALSE)
		FROM app.groups group_row
		JOIN app.group_database_grants grant_row
			ON grant_row.group_id = group_row.id
		LEFT JOIN app.users leader_user
			ON leader_user.id = group_row.leader_user_id
		LEFT JOIN app.group_memberships membership
			ON membership.group_id = group_row.id
		LEFT JOIN app.users member_user
			ON member_user.id = membership.user_id
		WHERE grant_row.database_id = $1
			AND grant_row.access IN ('view', 'edit')
		ORDER BY
			group_row.id,
			grant_row.access,
			membership.user_id
	`, databaseID)

	if err != nil {
		return [32]byte{}, err
	}
	defer rows.Close()

	type member struct {
		UserID string `json:"user_id"`
		Active bool   `json:"active"`
	}

	type roster struct {
		Group        string   `json:"group"`
		Leader       *string  `json:"leader"`
		LeaderActive bool     `json:"leader_active"`
		Access       Access   `json:"access"`
		Members      []member `json:"members"`
	}

	var (
		all     []roster
		current *roster
	)

	for rows.Next() {
		var (
			groupID      uuid.UUID
			leaderID     *uuid.UUID
			leaderActive bool
			access       Access
			memberID     *uuid.UUID
			memberActive bool
		)

		if err := rows.Scan(
			&groupID,
			&leaderID,
			&leaderActive,
			&access,
			&memberID,
			&memberActive,
		); err != nil {
			return [32]byte{}, err
		}

		groupKey := groupID.String()

		if current == nil || current.Group != groupKey {
			var leader *string

			if leaderID != nil {
				value := leaderID.String()
				leader = &value
			}

			all = append(all, roster{
				Group:        groupKey,
				Leader:       leader,
				LeaderActive: leaderActive,
				Access:       access,
				Members:      make([]member, 0),
			})

			current = &all[len(all)-1]
		}

		if memberID != nil {
			current.Members = append(current.Members, member{
				UserID: memberID.String(),
				Active: memberActive,
			})
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

// MayApprove memastikan aktor adalah ketua aktif dan anggota aktif
// dari grup yang diminta, serta approval grup tersebut masih pending.
func (p *Policy) MayApprove(
	ctx context.Context,
	actorID, proposalID, groupID uuid.UUID,
) (bool, error) {
	if p == nil || p.db == nil ||
		actorID == uuid.Nil ||
		proposalID == uuid.Nil ||
		groupID == uuid.Nil {
		return false, ErrForbidden
	}

	var allowed bool

	err := p.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM app.migration_proposals proposal
			JOIN app.group_database_grants grant_row
				ON grant_row.database_id = proposal.database_id
				AND grant_row.group_id = $3
				AND grant_row.access IN ('view', 'edit')
			JOIN app.groups group_row
				ON group_row.id = grant_row.group_id
				AND group_row.leader_user_id = $1
			JOIN app.users leader_user
				ON leader_user.id = group_row.leader_user_id
				AND leader_user.is_active
			WHERE proposal.id = $2
				AND proposal.status = 'pending_review'
				AND EXISTS (
					SELECT 1
					FROM app.group_memberships membership
					WHERE membership.group_id = group_row.id
						AND membership.user_id = $1
				)
				AND EXISTS (
					SELECT 1
					FROM app.migration_proposal_approvals approval
					WHERE approval.proposal_id = proposal.id
						AND approval.group_id = group_row.id
						AND approval.approval_round = (
							SELECT MAX(latest.approval_round)
							FROM app.migration_proposal_approvals latest
							WHERE latest.proposal_id = proposal.id
								AND latest.group_id = group_row.id
						)
						AND approval.leader_user_id = $1
						AND approval.status = 'pending'
				)
		)
	`, actorID, proposalID, groupID).Scan(&allowed)

	return allowed, err
}

// ApprovalSummary menghitung approval terbaru untuk setiap grup
// yang memiliki akses view atau edit terhadap database proposal.
func (p *Policy) ApprovalSummary(
	ctx context.Context,
	proposalID uuid.UUID,
) (ApprovalSummary, error) {
	if p == nil || p.db == nil || proposalID == uuid.Nil {
		return ApprovalSummary{}, ErrForbidden
	}

	rows, err := p.db.Query(ctx, `
		SELECT
			group_row.id,
			group_row.leader_user_id,
			COALESCE(
				group_row.leader_user_id IS NOT NULL
				AND leader_user.is_active
				AND EXISTS (
					SELECT 1
					FROM app.group_memberships membership
					WHERE membership.group_id = group_row.id
						AND membership.user_id = group_row.leader_user_id
				)
				AND latest_approval.status = 'approved'
				AND latest_approval.leader_user_id = group_row.leader_user_id,
				FALSE
			) AS approved
		FROM app.migration_proposals proposal
		JOIN app.group_database_grants grant_row
			ON grant_row.database_id = proposal.database_id
			AND grant_row.access IN ('view', 'edit')
		JOIN app.groups group_row
			ON group_row.id = grant_row.group_id
		LEFT JOIN app.users leader_user
			ON leader_user.id = group_row.leader_user_id
		LEFT JOIN LATERAL (
			SELECT
				approval.leader_user_id,
				approval.status
			FROM app.migration_proposal_approvals approval
			WHERE approval.proposal_id = proposal.id
				AND approval.group_id = group_row.id
			ORDER BY approval.approval_round DESC
			LIMIT 1
		) latest_approval ON TRUE
		WHERE proposal.id = $1
		ORDER BY group_row.id
	`, proposalID)

	if err != nil {
		return ApprovalSummary{}, err
	}
	defer rows.Close()

	var summary ApprovalSummary

	for rows.Next() {
		var requirement ApprovalRequirement

		if err := rows.Scan(
			&requirement.GroupID,
			&requirement.LeaderUserID,
			&requirement.Approved,
		); err != nil {
			return ApprovalSummary{}, err
		}

		summary.Required++

		if requirement.Approved {
			summary.Approved++
		}
	}

	if err := rows.Err(); err != nil {
		return ApprovalSummary{}, err
	}

	return summary, nil
}
