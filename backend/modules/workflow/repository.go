package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

func (r *Repository) LoadDraft(ctx context.Context, databaseID, migrationID uuid.UUID) (Snapshot, error) {
	var snapshot Snapshot
	var batch []byte
	err := r.db.QueryRow(ctx, `SELECT id::text,database_id::text,title,sql,sql_down,irreversible_reason,revision,mode,batch
		FROM app.migration_drafts WHERE id=$1 AND database_id=$2 FOR UPDATE`, migrationID, databaseID).
		Scan(&snapshot.MigrationID, &snapshot.DatabaseID, &snapshot.Title, &snapshot.SQLUp,
			&snapshot.SQLDown, &snapshot.IrreversibleReason, &snapshot.Revision, &snapshot.Mode, &batch)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, err
	}
	if len(batch) > 0 {
		if err := json.Unmarshal(batch, &snapshot.Batch); err != nil {
			return Snapshot{}, err
		}
	}
	return snapshot, nil
}

func (r *Repository) Fingerprint(ctx context.Context) ([]uuid.UUID, [32]byte, error) {
	return r.FingerprintForDatabase(ctx, uuid.Nil)
}

func (r *Repository) FingerprintForDatabase(ctx context.Context, databaseID uuid.UUID) ([]uuid.UUID, [32]byte, error) {
	var rows pgx.Rows
	var err error
	if databaseID == uuid.Nil {
		rows, err = r.db.Query(ctx, `SELECT g.id,g.leader_user_id,m.user_id
			FROM app.groups g
			LEFT JOIN app.group_memberships m ON m.group_id=g.id
			ORDER BY g.id,m.user_id`)
	} else {
		rows, err = r.db.Query(ctx, `SELECT g.id,g.leader_user_id,m.user_id
			FROM app.groups g
			JOIN app.group_database_grants gr ON gr.group_id=g.id
			LEFT JOIN app.group_memberships m ON m.group_id=g.id
			WHERE gr.database_id=$1
			ORDER BY g.id,m.user_id`, databaseID)
	}
	if err != nil {
		return nil, [32]byte{}, err
	}
	defer rows.Close()
	return scanFingerprintRows(rows)
}

func scanFingerprintRows(rows pgx.Rows) ([]uuid.UUID, [32]byte, error) {
	type roster struct {
		Group   string   `json:"group"`
		Leader  *string  `json:"leader"`
		Members []string `json:"members"`
	}
	var all []roster
	var groups []uuid.UUID
	var current *roster
	for rows.Next() {
		var group uuid.UUID
		var leader, member *uuid.UUID
		if err := rows.Scan(&group, &leader, &member); err != nil {
			return nil, [32]byte{}, err
		}
		if current == nil || current.Group != group.String() {
			all = append(all, roster{Group: group.String()})
			current = &all[len(all)-1]
			groups = append(groups, group)
		}
		if leader != nil {
			value := leader.String()
			current.Leader = &value
		}
		if member != nil {
			current.Members = append(current.Members, member.String())
		}
	}
	if err := rows.Err(); err != nil {
		return nil, [32]byte{}, err
	}
	raw, err := json.Marshal(all)
	if err != nil {
		return nil, [32]byte{}, err
	}
	return groups, digestBytes(raw), nil
}

func digestBytes(raw []byte) [32]byte {
	return sha256.Sum256(raw)
}

func (r *Repository) CreateProposal(ctx context.Context, databaseID, migrationID, actorID uuid.UUID, revision int, action string, sourceExecutionID *uuid.UUID, snapshot Snapshot, snapshotHash, fingerprint [32]byte, validation []byte) (uuid.UUID, error) {
	id := uuid.New()
	_, err := r.db.Exec(ctx, `INSERT INTO app.migration_proposals
		(id,database_id,migration_id,revision,action,source_execution_id,status,snapshot,snapshot_hash,approval_fingerprint,validation,created_by)
		VALUES($1,$2,$3,$4,$5,$6,'draft',$7,$8,$9,$10,$11)`,
		id, databaseID, migrationID, revision, action, sourceExecutionID, snapshot, snapshotHash[:], fingerprint[:], validation, actorID)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

func (r *Repository) Submit(ctx context.Context, id, actorID uuid.UUID) (Proposal, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return Proposal{}, err
	}
	defer tx.Rollback(ctx)
	var databaseID, migrationID uuid.UUID
	var revision int
	var status string
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT database_id,migration_id,revision,status,snapshot
		FROM app.migration_proposals WHERE id=$1 FOR UPDATE`, id).
		Scan(&databaseID, &migrationID, &revision, &status, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, ErrNotFound
	}
	if err != nil {
		return Proposal{}, err
	}
	if status != "draft" {
		return Proposal{}, ErrConflict
	}
	var currentRevision int
	var sqlDown, reason *string
	err = tx.QueryRow(ctx, `SELECT revision,sql_down,irreversible_reason
		FROM app.migration_drafts WHERE id=$1 AND database_id=$2 FOR UPDATE`, migrationID, databaseID).
		Scan(&currentRevision, &sqlDown, &reason)
	if errors.Is(err, pgx.ErrNoRows) || currentRevision != revision {
		return Proposal{}, ErrProposalStale
	}
	if err != nil {
		return Proposal{}, err
	}
	groups, fingerprint, err := fingerprintTx(ctx, tx, databaseID)
	if err != nil {
		return Proposal{}, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return Proposal{}, err
	}
	snapshot.SQLDown, snapshot.IrreversibleReason = sqlDown, reason
	validation := validateSnapshot(snapshot)
	validationJSON, _ := json.Marshal(validation)
	if _, err = tx.Exec(ctx, `UPDATE app.migration_proposals
		SET status='pending_review',approval_fingerprint=$2,validation=$3 WHERE id=$1`,
		id, fingerprint[:], validationJSON); err != nil {
		return Proposal{}, err
	}
	leaders := make(map[uuid.UUID]*uuid.UUID)
	for _, groupID := range groups {
		var leader *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT leader_user_id FROM app.groups WHERE id=$1`, groupID).Scan(&leader); err != nil {
			return Proposal{}, err
		}
		leaders[groupID] = leader
		if _, err = tx.Exec(ctx, `INSERT INTO app.migration_proposal_approvals
			(proposal_id,group_id,approval_round,leader_user_id)
			VALUES($1,$2,1,$3)`, id, groupID, leader); err != nil {
			return Proposal{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Proposal{}, err
	}
	_ = actorID
	_ = leaders
	return r.Get(ctx, id)
}

func (r *Repository) Approve(ctx context.Context, proposalID, actorID uuid.UUID) (Proposal, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return Proposal{}, err
	}
	defer tx.Rollback(ctx)
	var databaseID, migrationID uuid.UUID
	var revision int
	var status string
	var stored []byte
	err = tx.QueryRow(ctx, `SELECT database_id,migration_id,revision,status,approval_fingerprint
		FROM app.migration_proposals WHERE id=$1 FOR UPDATE`, proposalID).
		Scan(&databaseID, &migrationID, &revision, &status, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, ErrNotFound
	}
	if err != nil {
		return Proposal{}, err
	}
	if status != "pending_review" {
		return Proposal{}, ErrConflict
	}
	var currentRevision int
	if err = tx.QueryRow(ctx, `SELECT revision FROM app.migration_drafts WHERE id=$1`, migrationID).Scan(&currentRevision); err != nil {
		return Proposal{}, err
	}
	_, fingerprint, err := fingerprintTx(ctx, tx, databaseID)
	if err != nil {
		return Proposal{}, err
	}
	if currentRevision != revision || !equalBytes(stored, fingerprint[:]) {
		_, _ = tx.Exec(ctx, `UPDATE app.migration_proposals SET status='superseded' WHERE id=$1`, proposalID)
		return Proposal{}, ErrProposalStale
	}
	var groupID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT g.id FROM app.groups g
		JOIN app.group_database_grants gr ON gr.group_id=g.id
		WHERE gr.database_id=$1 AND g.leader_user_id=$2`, databaseID, actorID).Scan(&groupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, ErrConflict
	}
	if err != nil {
		return Proposal{}, err
	}
	var round int
	err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(approval_round),0)
		FROM app.migration_proposal_approvals WHERE proposal_id=$1 AND group_id=$2`, proposalID, groupID).Scan(&round)
	if err != nil {
		return Proposal{}, err
	}
	if round == 0 {
		return Proposal{}, ErrConflict
	}
	tag, err := tx.Exec(ctx, `UPDATE app.migration_proposal_approvals
		SET status='approved',leader_user_id=$3
		WHERE proposal_id=$1 AND group_id=$2 AND approval_round=$4 AND status='pending'`,
		proposalID, groupID, actorID, round)
	if err != nil {
		return Proposal{}, err
	}
	if tag.RowsAffected() != 1 {
		return Proposal{}, ErrConflict
	}
	var required, approved int
	err = tx.QueryRow(ctx, `SELECT COUNT(*),COUNT(*) FILTER (WHERE a.status='approved')
		FROM app.group_database_grants gr
		JOIN app.migration_proposal_approvals a
		  ON a.group_id=gr.group_id AND a.proposal_id=$1
		WHERE gr.database_id=$2 AND a.approval_round=$3`, proposalID, databaseID, round).Scan(&required, &approved)
	if err != nil {
		return Proposal{}, err
	}
	if required > 0 && required == approved {
		if _, err = tx.Exec(ctx, `UPDATE app.migration_proposals SET status='approved'
			WHERE id=$1 AND status='pending_review'`, proposalID); err != nil {
			return Proposal{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Proposal{}, err
	}
	return r.Get(ctx, proposalID)
}

func (r *Repository) Get(ctx context.Context, id uuid.UUID) (Proposal, error) {
	var proposal Proposal
	var databaseID, migrationID uuid.UUID
	var snapJSON, validationJSON []byte
	var hash, fingerprint []byte
	err := r.db.QueryRow(ctx, `SELECT id,database_id,migration_id,revision,action,source_execution_id,status,
		snapshot,snapshot_hash,approval_fingerprint,validation
		FROM app.migration_proposals WHERE id=$1`, id).
		Scan(&proposal.ProposalID, &databaseID, &migrationID, &proposal.Revision, &proposal.Action,
			&proposal.SourceExecutionID, &proposal.Status, &snapJSON, &hash, &fingerprint, &validationJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, ErrNotFound
	}
	if err != nil {
		return Proposal{}, err
	}
	proposal.DatabaseID = databaseID.String()
	proposal.MigrationID = migrationID.String()
	proposal.ProposalID = id.String()
	proposal.SnapshotHash = hexEncode(hash)
	proposal.ApprovalFingerprint = hexEncode(fingerprint)
	if err := json.Unmarshal(snapJSON, &proposal.Snapshot); err != nil {
		return Proposal{}, err
	}
	if len(validationJSON) > 0 {
		if err := json.Unmarshal(validationJSON, &proposal.Validation); err != nil {
			return Proposal{}, err
		}
	}
	rows, err := r.db.Query(ctx, `SELECT a.group_id,a.leader_user_id,a.status,a.approved_at
		FROM app.migration_proposal_approvals a
		WHERE a.proposal_id=$1
		  AND a.approval_round=(
			SELECT COALESCE(MAX(x.approval_round),1)
			FROM app.migration_proposal_approvals x
			WHERE x.proposal_id=a.proposal_id AND x.group_id=a.group_id
		  )
		ORDER BY a.group_id`, id)
	if err != nil {
		return Proposal{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var approval Approval
		if err := rows.Scan(&approval.GroupID, &approval.LeaderUserID, &approval.Status, &approval.ApprovedAt); err != nil {
			return Proposal{}, err
		}
		proposal.Approvals = append(proposal.Approvals, approval)
	}
	if err := rows.Err(); err != nil {
		return Proposal{}, err
	}
	return proposal, nil
}

func fingerprintTx(ctx context.Context, tx pgx.Tx, databaseID uuid.UUID) ([]uuid.UUID, [32]byte, error) {
	rows, err := tx.Query(ctx, `SELECT g.id,g.leader_user_id,m.user_id
		FROM app.groups g
		JOIN app.group_database_grants gr ON gr.group_id=g.id
		LEFT JOIN app.group_memberships m ON m.group_id=g.id
		WHERE gr.database_id=$1
		ORDER BY g.id,m.user_id`, databaseID)
	if err != nil {
		return nil, [32]byte{}, err
	}
	defer rows.Close()
	return scanFingerprintRows(rows)
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func hexEncode(value []byte) string {
	return hex.EncodeToString(value)
}
