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
type fingerprintMember struct {
	UserID string `json:"user_id"`
	Active bool   `json:"active"`
}

type fingerprintRoster struct {
	Group        string              `json:"group"`
	Leader       *string             `json:"leader"`
	LeaderActive bool                `json:"leader_active"`
	Access       Access              `json:"access"`
	Members      []fingerprintMember `json:"members"`
}

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
	rows, err := r.db.Query(ctx, `
		SELECT
			group_row.id,
			group_row.leader_user_id,
			COALESCE(leader_user.is_active, FALSE),
			''::text AS access,
			membership.user_id,
			COALESCE(member_user.is_active, FALSE)
		FROM app.groups group_row
		LEFT JOIN app.users leader_user
			ON leader_user.id = group_row.leader_user_id
		LEFT JOIN app.group_memberships membership
			ON membership.group_id = group_row.id
		LEFT JOIN app.users member_user
			ON member_user.id = membership.user_id
		ORDER BY group_row.id, membership.user_id
	`)
	if err != nil {
		return nil, [32]byte{}, err
	}
	defer rows.Close()

	return scanFingerprintRows(rows)
}

func (r *Repository) FingerprintForDatabase(
	ctx context.Context,
	databaseID uuid.UUID,
) ([]uuid.UUID, [32]byte, error) {
	rows, err := r.db.Query(ctx, `
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
		return nil, [32]byte{}, err
	}
	defer rows.Close()

	return scanFingerprintRows(rows)
}


func scanFingerprintRows(
	rows pgx.Rows,
) ([]uuid.UUID, [32]byte, error) {
	var (
		all     []fingerprintRoster
		groups  []uuid.UUID
		current *fingerprintRoster
	)

	for rows.Next() {
		var (
			groupID      uuid.UUID
			leaderID     *uuid.UUID
			leaderActive bool
			access       string
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
			return nil, [32]byte{}, err
		}

		groupKey := groupID.String()

		if current == nil || current.Group != groupKey {
			var leader *string

			if leaderID != nil {
				value := leaderID.String()
				leader = &value
			}

			all = append(all, fingerprintRoster{
				Group:        groupKey,
				Leader:       leader,
				LeaderActive: leaderActive,
				Access:       Access(access),
				Members:      make([]fingerprintMember, 0),
			})

			current = &all[len(all)-1]
			groups = append(groups, groupID)
		}

		if memberID != nil {
			current.Members = append(current.Members, fingerprintMember{
				UserID: memberID.String(),
				Active: memberActive,
			})
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

func (r *Repository) CreateProposal(
	ctx context.Context,
	databaseID, migrationID, actorID uuid.UUID,
	revision int,
	action string,
	sourceExecutionID *uuid.UUID,
	snapshot Snapshot,
	snapshotHash, fingerprint [32]byte,
	_ []byte,
) (uuid.UUID, error) {
	id := uuid.New()

	_, err := r.db.Exec(ctx, `
		INSERT INTO app.migration_proposals (
			id,
			database_id,
			migration_id,
			revision,
			action,
			source_execution_id,
			status,
			snapshot,
			snapshot_hash,
			approval_fingerprint,
			validation,
			created_by
		)
		VALUES (
			$1, $2, $3, $4, $5, $6,
			'draft', $7, $8, $9, NULL, $10
		)
	`,
		id,
		databaseID,
		migrationID,
		revision,
		action,
		sourceExecutionID,
		snapshot,
		snapshotHash[:],
		fingerprint[:],
		actorID,
	)
	if err != nil {
		return uuid.Nil, err
	}

	return id, nil
}

func (r *Repository) Submit(
	ctx context.Context,
	id, actorID uuid.UUID,
) (Proposal, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return Proposal{}, err
	}
	defer tx.Rollback(ctx)

	var (
		databaseID  uuid.UUID
		migrationID uuid.UUID
		revision    int
		status      string
		raw         []byte
	)

	err = tx.QueryRow(ctx, `
		SELECT
			database_id,
			migration_id,
			revision,
			status,
			snapshot
		FROM app.migration_proposals
		WHERE id = $1
		FOR UPDATE
	`, id).Scan(
		&databaseID,
		&migrationID,
		&revision,
		&status,
		&raw,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, ErrNotFound
	}
	if err != nil {
		return Proposal{}, err
	}
	if status != "draft" {
		return Proposal{}, ErrConflict
	}

	var canEdit bool

	err = tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
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
				AND grant_row.access = 'edit'
		)
	`, actorID, databaseID).Scan(&canEdit)

	if err != nil {
		return Proposal{}, err
	}
	if !canEdit {
		return Proposal{}, ErrForbidden
	}

	var (
		currentRevision int
		sqlDown         *string
		reason          *string
	)

	err = tx.QueryRow(ctx, `
		SELECT revision, sql_down, irreversible_reason
		FROM app.migration_drafts
		WHERE id = $1 AND database_id = $2
		FOR UPDATE
	`, migrationID, databaseID).Scan(
		&currentRevision,
		&sqlDown,
		&reason,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, ErrProposalStale
	}
	if err != nil {
		return Proposal{}, err
	}
	if currentRevision != revision {
		return Proposal{}, ErrProposalStale
	}

	groups, fingerprint, err := fingerprintTx(ctx, tx, databaseID)
	if err != nil {
		return Proposal{}, err
	}
	if len(groups) == 0 {
		return Proposal{}, ErrForbidden
	}

	var snapshot Snapshot

	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return Proposal{}, err
	}

	snapshot.SQLDown = sqlDown
	snapshot.IrreversibleReason = reason

	validation := validateSnapshot(snapshot)

	switch validation.Status {
	case "valid":
		validation.Status = "passed"
	case "invalid":
		validation.Status = "failed"
	}

	validationJSON, err := json.Marshal(validation)
	if err != nil {
		return Proposal{}, err
	}

	// Validasi gagal: simpan hasilnya, tetapi jangan submit proposal.
	if validation.Status != "passed" {
		_, err = tx.Exec(ctx, `
			UPDATE app.migration_proposals
			SET validation = $2
			WHERE id = $1
				AND status = 'draft'
		`, id, validationJSON)

		if err != nil {
			return Proposal{}, err
		}

		if err = tx.Commit(ctx); err != nil {
			return Proposal{}, err
		}

		proposal, getErr := r.Get(ctx, id)
		if getErr != nil {
			return Proposal{}, getErr
		}

		return proposal, ErrInvalidProposal
	}

	_, err = tx.Exec(ctx, `
		UPDATE app.migration_proposals
		SET
			status = 'pending_review',
			approval_fingerprint = $2,
			validation = $3
		WHERE id = $1
			AND status = 'draft'
	`, id, fingerprint[:], validationJSON)

	if err != nil {
		return Proposal{}, err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO app.migration_proposal_approvals (
			proposal_id,
			group_id,
			approval_round,
			leader_user_id
		)
		SELECT
			$1,
			group_row.id,
			1,
			group_row.leader_user_id
		FROM app.groups group_row
		JOIN app.group_database_grants grant_row
			ON grant_row.group_id = group_row.id
		WHERE grant_row.database_id = $2
			AND grant_row.access IN ('view', 'edit')
	`, id, databaseID)

	if err != nil {
		return Proposal{}, err
	}

	if err = tx.Commit(ctx); err != nil {
		return Proposal{}, err
	}

	return r.Get(ctx, id)
}

func (r *Repository) Approve(
	ctx context.Context,
	proposalID, actorID, groupID uuid.UUID,
	requestedRevision int,
) (Proposal, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return Proposal{}, err
	}
	defer tx.Rollback(ctx)

	var (
		databaseID        uuid.UUID
		migrationID       uuid.UUID
		revision          int
		status            string
		storedFingerprint []byte
	)

	err = tx.QueryRow(ctx, `
		SELECT
			database_id,
			migration_id,
			revision,
			status,
			approval_fingerprint
		FROM app.migration_proposals
		WHERE id = $1
		FOR UPDATE
	`, proposalID).Scan(
		&databaseID,
		&migrationID,
		&revision,
		&status,
		&storedFingerprint,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, ErrNotFound
	}
	if err != nil {
		return Proposal{}, err
	}
	if status != "pending_review" {
		return Proposal{}, ErrConflict
	}
	if requestedRevision != revision {
		return Proposal{}, ErrProposalStale
	}

	var currentRevision int

	err = tx.QueryRow(ctx, `
		SELECT revision
		FROM app.migration_drafts
		WHERE id = $1 AND database_id = $2
		FOR UPDATE
	`, migrationID, databaseID).Scan(&currentRevision)

	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, ErrProposalStale
	}
	if err != nil {
		return Proposal{}, err
	}

	// Periksa fingerprint terbaru.
	_, currentFingerprint, err := fingerprintTx(ctx, tx, databaseID)
	if err != nil {
		return Proposal{}, err
	}

	if currentRevision != revision ||
		!equalBytes(storedFingerprint, currentFingerprint[:]) {

		_, err = tx.Exec(ctx, `
			UPDATE app.migration_proposals
			SET status = 'superseded'
			WHERE id = $1
				AND status = 'pending_review'
		`, proposalID)

		if err != nil {
			return Proposal{}, err
		}

		if err = tx.Commit(ctx); err != nil {
			return Proposal{}, err
		}

		return Proposal{}, ErrProposalStale
	}

	var anchorValid bool

	err = tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM app.groups group_row
			JOIN app.group_database_grants grant_row
				ON grant_row.group_id = group_row.id
				AND grant_row.database_id = $3
				AND grant_row.access IN ('view', 'edit')
			JOIN app.users leader_user
				ON leader_user.id = group_row.leader_user_id
				AND leader_user.is_active
			JOIN app.group_memberships membership
				ON membership.group_id = group_row.id
				AND membership.user_id = $1
			WHERE group_row.id = $2
				AND group_row.leader_user_id = $1
		)
	`, actorID, groupID, databaseID).Scan(&anchorValid)

	if err != nil {
		return Proposal{}, err
	}
	if !anchorValid {
		return Proposal{}, ErrForbidden
	}

	rows, err := tx.Query(ctx, `
		SELECT group_row.id
		FROM app.groups group_row
		JOIN app.group_database_grants grant_row
			ON grant_row.group_id = group_row.id
		JOIN app.users leader_user
			ON leader_user.id = group_row.leader_user_id
			AND leader_user.is_active
		JOIN app.group_memberships membership
			ON membership.group_id = group_row.id
			AND membership.user_id = $1
		WHERE grant_row.database_id = $2
			AND grant_row.access IN ('view', 'edit')
			AND group_row.leader_user_id = $1
		ORDER BY group_row.id
	`, actorID, databaseID)

	if err != nil {
		return Proposal{}, err
	}

	var ledGroups []uuid.UUID

	for rows.Next() {
		var currentGroupID uuid.UUID

		if err := rows.Scan(&currentGroupID); err != nil {
			rows.Close()
			return Proposal{}, err
		}

		ledGroups = append(ledGroups, currentGroupID)
	}

	if err := rows.Err(); err != nil {
		rows.Close()
		return Proposal{}, err
	}
	rows.Close()

	if len(ledGroups) == 0 {
		return Proposal{}, ErrForbidden
	}

	anchorFound := false

	for _, currentGroupID := range ledGroups {
		if currentGroupID == groupID {
			anchorFound = true
			break
		}
	}

	if !anchorFound {
		return Proposal{}, ErrForbidden
	}

	for _, currentGroupID := range ledGroups {
		tag, err := tx.Exec(ctx, `
			UPDATE app.migration_proposal_approvals approval
			SET
				status = 'approved',
				approved_at = clock_timestamp(),
				updated_at = clock_timestamp()
			WHERE approval.proposal_id = $1
				AND approval.group_id = $2
				AND approval.leader_user_id = $3
				AND approval.status = 'pending'
				AND approval.approval_round = (
					SELECT MAX(latest.approval_round)
					FROM app.migration_proposal_approvals latest
					WHERE latest.proposal_id = $1
						AND latest.group_id = $2
				)
		`, proposalID, currentGroupID, actorID)

		if err != nil {
			return Proposal{}, err
		}

		if tag.RowsAffected() != 1 {
			return Proposal{}, ErrConflict
		}
	}

	// Hitung approval terbaru untuk setiap grup secara independen.
	var required, approved int

	err = tx.QueryRow(ctx, `
		SELECT
			COUNT(*),
			COUNT(*) FILTER (
				WHERE latest_approval.status = 'approved'
					AND latest_approval.leader_user_id = group_row.leader_user_id
					AND group_row.leader_user_id IS NOT NULL
					AND leader_user.is_active
					AND EXISTS (
						SELECT 1
						FROM app.group_memberships membership
						WHERE membership.group_id = group_row.id
							AND membership.user_id = group_row.leader_user_id
					)
			)
		FROM app.group_database_grants grant_row
		JOIN app.groups group_row
			ON group_row.id = grant_row.group_id
		LEFT JOIN app.users leader_user
			ON leader_user.id = group_row.leader_user_id
		LEFT JOIN LATERAL (
			SELECT
				approval.status,
				approval.leader_user_id
			FROM app.migration_proposal_approvals approval
			WHERE approval.proposal_id = $1
				AND approval.group_id = group_row.id
			ORDER BY approval.approval_round DESC
			LIMIT 1
		) latest_approval ON TRUE
		WHERE grant_row.database_id = $2
			AND grant_row.access IN ('view', 'edit')
	`, proposalID, databaseID).Scan(&required, &approved)

	if err != nil {
		return Proposal{}, err
	}

	if required > 0 && required == approved {
		_, err = tx.Exec(ctx, `
			UPDATE app.migration_proposals
			SET status = 'approved'
			WHERE id = $1
				AND status = 'pending_review'
		`, proposalID)

		if err != nil {
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

func fingerprintTx(
	ctx context.Context,
	tx pgx.Tx,
	databaseID uuid.UUID,
) ([]uuid.UUID, [32]byte, error) {
	rows, err := tx.Query(ctx, `
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
