package migration

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/history"
)

type platformStore interface {
	lookup(context.Context, string, string) (attempt, error)
	binding(context.Context, string) (string, error)
	reserve(context.Context, Actor, string, string, string, ExecuteRequest, func(string) error) (attempt, bool, error)
	load(context.Context, string, string) (attempt, error)
	transition(context.Context, attempt, string, *history.ExecutionError) (attempt, error)
}

type sqlStore struct{ db *sql.DB }

const attemptColumns = `id::text,database_id::text,draft_id::text,draft_revision,
version::text,title,sql,executed_by::text,executed_username,started_at,finished_at,
status,error_code,error_message,execution_target_id::text,request_id::text,sql_sha256`

const attemptSelect = `SELECT ` + attemptColumns + ` FROM app.migration_executions`

type rowScanner interface{ Scan(...any) error }

func scanAttempt(row rowScanner) (attempt, error) {
	var a attempt
	var finished sql.NullTime
	var code, message sql.NullString
	err := row.Scan(&a.ID, &a.DatabaseID, &a.DraftID, &a.DraftRevision,
		&a.Version, &a.Title, &a.SQL, &a.ExecutedBy.ID, &a.ExecutedBy.Username,
		&a.StartedAt, &finished, &a.Status, &code, &message, &a.TargetID, &a.RequestID, &a.Hash)
	if err != nil {
		return attempt{}, err
	}
	if len(a.Hash) != 32 || !bytes.Equal(a.Hash, sqlHash(a.SQL)) {
		return attempt{}, errors.New("invalid stored SQL hash")
	}
	a.StartedAt = a.StartedAt.UTC()
	if finished.Valid {
		utc := finished.Time.UTC()
		a.FinishedAt = &utc
	}
	if code.Valid {
		a.Error = &history.ExecutionError{Code: code.String, Message: message.String}
	}
	return a, nil
}

func (s sqlStore) lookup(ctx context.Context, databaseID, requestID string) (attempt, error) {
	return scanAttempt(s.db.QueryRowContext(ctx, attemptSelect+` WHERE database_id=$1 AND request_id=$2`, databaseID, requestID))
}

func (s sqlStore) load(ctx context.Context, databaseID, executionID string) (attempt, error) {
	a, err := scanAttempt(s.db.QueryRowContext(ctx, attemptSelect+` WHERE database_id=$1 AND id=$2`, databaseID, executionID))
	if errors.Is(err, sql.ErrNoRows) {
		return attempt{}, fault(404, "NOT_FOUND", "Execution not found")
	}
	return a, err
}

func (s sqlStore) binding(ctx context.Context, databaseID string) (string, error) {
	var target sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT execution_target_id::text FROM app.database_connections WHERE id=$1 AND deleted_at IS NULL`, databaseID).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fault(404, "NOT_FOUND", "Database not found")
	}
	if err != nil {
		return "", err
	}
	if !target.Valid {
		return "", fault(502, "TARGET_UNAVAILABLE", "Execution target is not ready")
	}
	return target.String, nil
}

func (s sqlStore) reserve(ctx context.Context, actor Actor, databaseID, draftID, targetID string, in ExecuteRequest, prepare func(string) error) (attempt, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return attempt{}, false, err
	}
	defer tx.Rollback()
	var bound sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT execution_target_id::text FROM app.database_connections WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, databaseID).Scan(&bound)
	if errors.Is(err, sql.ErrNoRows) {
		return attempt{}, false, fault(404, "NOT_FOUND", "Database not found")
	}
	if err != nil {
		return attempt{}, false, err
	}
	if !bound.Valid || bound.String != targetID {
		return attempt{}, false, fault(409, "DATABASE_BUSY", "Target binding has changed")
	}
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT next_version FROM app.migration_target_state WHERE id=$1 FOR UPDATE`, targetID).Scan(&next); err != nil {
		return attempt{}, false, err
	}
	existing, err := scanAttempt(tx.QueryRowContext(ctx, attemptSelect+` WHERE database_id=$1 AND request_id=$2`, databaseID, in.RequestID))
	if err == nil {
		if err := sameIntent(existing, actor, draftID, in); err != nil {
			return attempt{}, false, err
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return attempt{}, false, err
	}
	var title, sqlText string
	var revision int
	var applied sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT title,sql,revision,applied_execution_id::text FROM app.migration_drafts WHERE database_id=$1 AND id=$2 FOR UPDATE`, databaseID, draftID).Scan(&title, &sqlText, &revision, &applied)
	if errors.Is(err, sql.ErrNoRows) {
		return attempt{}, false, fault(404, "NOT_FOUND", "Draft not found")
	}
	if err != nil {
		return attempt{}, false, err
	}
	if applied.Valid {
		return attempt{}, false, fault(409, "ALREADY_APPLIED", "Draft has already been applied")
	}
	if revision != in.Revision {
		return attempt{}, false, fault(409, "VERSION_CONFLICT", "Draft revision has changed")
	}
	var busy bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM app.migration_executions WHERE (execution_target_id=$1 OR draft_id=$2) AND status IN ('running','unknown'))`, targetID, draftID).Scan(&busy)
	if err != nil {
		return attempt{}, false, err
	}
	if busy {
		return attempt{}, false, fault(409, "DATABASE_BUSY", "Target has an unresolved execution")
	}
	if err := prepare(sqlText); err != nil {
		return attempt{}, false, err
	}
	var username string
	err = tx.QueryRowContext(ctx, `SELECT username FROM app.users WHERE id=$1 AND is_active`, actor.ID).Scan(&username)
	if errors.Is(err, sql.ErrNoRows) {
		return attempt{}, false, fault(403, "FORBIDDEN", "Actor is no longer active")
	}
	if err != nil {
		return attempt{}, false, err
	}
	var highest int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM app.migration_executions WHERE database_id=$1`, databaseID).Scan(&highest); err != nil {
		return attempt{}, false, err
	}
	if highest == math.MaxInt64 || next == math.MaxInt64 {
		return attempt{}, false, fault(409, "VERSION_CONFLICT", "Migration version is exhausted")
	}
	version := next
	if highest >= version {
		version = highest + 1
	}
	if version >= math.MaxInt64 {
		return attempt{}, false, fault(409, "VERSION_CONFLICT", "Migration version is exhausted")
	}
	id, err := newUUID()
	if err != nil {
		return attempt{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE app.migration_target_state SET next_version=$2 WHERE id=$1`, targetID, version+1); err != nil {
		return attempt{}, false, err
	}
	a, err := scanAttempt(tx.QueryRowContext(ctx, `INSERT INTO app.migration_executions
(id,database_id,execution_target_id,draft_id,request_id,draft_revision,version,title,sql,sql_sha256,executed_by,executed_username,started_at,status)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,clock_timestamp(),'running') RETURNING `+attemptColumns,
		id, databaseID, targetID, draftID, in.RequestID, revision, version, title, sqlText, sqlHash(sqlText), actor.ID, username))
	if err != nil {
		return attempt{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return attempt{}, false, err
	}
	return a, true, nil
}

func (s sqlStore) transition(ctx context.Context, expected attempt, status string, failure *history.ExecutionError) (attempt, error) {
	if status != "succeeded" && status != "failed" && status != "unknown" {
		return attempt{}, errors.New("invalid execution transition")
	}
	if status == "failed" && failure == nil {
		return attempt{}, errors.New("failure evidence is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return attempt{}, err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id::text FROM app.database_connections WHERE id=$1 FOR UPDATE`, expected.DatabaseID).Scan(&id); err != nil {
		return attempt{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT id::text FROM app.migration_target_state WHERE id=$1 FOR UPDATE`, expected.TargetID).Scan(&id); err != nil {
		return attempt{}, err
	}
	var applied sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT applied_execution_id::text FROM app.migration_drafts WHERE database_id=$1 AND id=$2 FOR UPDATE`, expected.DatabaseID, expected.DraftID).Scan(&applied); err != nil {
		return attempt{}, err
	}
	current, err := scanAttempt(tx.QueryRowContext(ctx, attemptSelect+` WHERE database_id=$1 AND id=$2 FOR UPDATE`, expected.DatabaseID, expected.ID))
	if err != nil {
		return attempt{}, err
	}
	if current.TargetID != expected.TargetID || current.Version != expected.Version || !bytes.Equal(current.Hash, expected.Hash) {
		return attempt{}, errors.New("execution snapshot mismatch")
	}
	if current.Status == status {
		return current, nil
	}
	if terminal(current.Status) {
		return current, fault(409, "REQUEST_CONFLICT", "Execution outcome is already final")
	}
	var code, message any
	if status != "succeeded" && failure != nil {
		code, message = failure.Code, failure.Message
	}
	updated, err := scanAttempt(tx.QueryRowContext(ctx, `UPDATE app.migration_executions
SET status=$3,finished_at=CASE WHEN $3='unknown' THEN NULL ELSE GREATEST(clock_timestamp(),started_at) END,error_code=$4,error_message=$5
WHERE database_id=$1 AND id=$2 RETURNING `+attemptColumns, expected.DatabaseID, expected.ID, status, code, message))
	if err != nil {
		return attempt{}, err
	}
	if status == "succeeded" {
		if applied.Valid && applied.String != expected.ID {
			return attempt{}, fault(409, "REQUEST_CONFLICT", "Draft has another applied execution")
		}
		if !applied.Valid {
			result, err := tx.ExecContext(ctx, `UPDATE app.migration_drafts SET applied_execution_id=$3 WHERE database_id=$1 AND id=$2`, expected.DatabaseID, expected.DraftID, expected.ID)
			if err != nil {
				return attempt{}, err
			}
			count, err := result.RowsAffected()
			if err != nil || count != 1 {
				return attempt{}, errors.New("applied draft update failed")
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return attempt{}, err
	}
	return updated, nil
}

func numericVersion(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }
