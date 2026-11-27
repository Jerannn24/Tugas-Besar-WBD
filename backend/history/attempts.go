package history

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
)

type historyCursor struct {
	Database string    `json:"d"`
	Hide     bool      `json:"h"`
	Started  time.Time `json:"t"`
	Attempt  string    `json:"a"`
}

// commitProjection is evaluated only for rows on the requested page.
const commitProjection = `SELECT jsonb_build_object(
 'id',h.execution_id,'execution_id',h.execution_id,'database_id',h.database_id,
 'draft_id',h.migration_id,'migration_id',h.migration_id,'draft_revision',h.revision,'revision',h.revision,
 'version',h.target_version::text,'title',j.title,'sql',h.sql_snapshot,'sql_snapshot',h.sql_snapshot,
 'action',h.action,'direction',h.direction,'executed_by',jsonb_build_object('id',h.executed_by,'username',h.executed_username),
 'started_at',a.started_at,'finished_at',a.finished_at,'status',a.status,
 'error',CASE WHEN a.error_code IS NOT NULL THEN jsonb_build_object('code',a.error_code,'message','Migration attempt did not complete') END,
 'committed_at',h.committed_at,'source_execution_id',h.source_execution_id,'reverts_execution_id',h.reverts_execution_id,
 'is_complete',h.is_complete,'batch_commits',COALESCE((
 SELECT jsonb_agg(jsonb_build_object('batch_index',b.batch_index::text,'row_count',b.row_count::text,
 'committed_at',b.committed_at,'receipt',b.receipt,'range_start',b.range_start,'range_end',b.range_end,
 'sql_snapshot',b.sql_snapshot,'safe_parameters',b.safe_parameters,'job_id',b.job_id,
 'executed_by',jsonb_build_object('id',b.executed_by,'username',b.executed_username)) ORDER BY b.batch_index)
 FROM app.migration_history_entries b WHERE b.database_id=h.database_id AND b.execution_id=h.execution_id AND b.batch_index IS NOT NULL `

const commitProjectionEnd = ` ),'[]'::jsonb))
 FROM app.migration_history_entries h JOIN app.jobs j ON j.id=h.job_id
 JOIN app.job_attempts a ON a.id=h.attempt_id `

func (h *Handler) commit(ctx context.Context, database, execution string) (json.RawMessage, error) {
	var data json.RawMessage
	err := h.d.Platform.QueryRowContext(ctx, commitProjection+commitProjectionEnd+` WHERE h.database_id=$1 AND h.execution_id=$2 ORDER BY h.version DESC LIMIT 1`, database, execution).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return data, err
}

var attemptPageQuery = `WITH candidates AS (
 SELECT a.id AS attempt_id,a.execution_id,a.migration_id,a.revision,a.actor_id,u.username,
 j.action,j.direction,a.status,a.started_at,a.finished_at,j.source_execution_id,a.sql_entered,
 (SELECT h.target_version::text FROM app.migration_history_entries h WHERE h.database_id=a.database_id AND h.attempt_id=a.id ORDER BY h.version DESC LIMIT 1) AS version,
 a.database_id,NULL::jsonb AS legacy_commit,NULL::text AS legacy_sql
 FROM app.job_attempts a JOIN app.jobs j ON j.id=a.job_id JOIN app.users u ON u.id=a.actor_id
 WHERE a.database_id=$1
 UNION ALL
 SELECT e.id,e.id,e.draft_id,e.draft_revision,e.executed_by,e.executed_username,
 'apply','up',e.status,e.started_at,e.finished_at,NULL::uuid,NULL::boolean,
 CASE WHEN e.status='succeeded' THEN e.version::text END,e.database_id,
 CASE WHEN e.status='succeeded' THEN jsonb_build_object(
 'id',e.id,'execution_id',e.id,'database_id',e.database_id,'draft_id',e.draft_id,'migration_id',e.draft_id,
 'draft_revision',e.draft_revision,'revision',e.draft_revision,'version',e.version::text,'title',e.title,
 'sql',e.sql,'sql_snapshot',e.sql,'action','apply','direction','up',
 'executed_by',jsonb_build_object('id',e.executed_by,'username',e.executed_username),
 'started_at',e.started_at,'finished_at',e.finished_at,'status',e.status,'error',NULL,
 'committed_at',COALESCE(e.finished_at,e.started_at),'source_execution_id',NULL,'reverts_execution_id',NULL,
 'is_complete',true,'batch_commits','[]'::jsonb) END,e.sql
 FROM app.migration_executions e WHERE e.database_id=$1
 AND NOT EXISTS (SELECT 1 FROM app.jobs j WHERE j.database_id=e.database_id AND j.execution_id=e.id)
), page AS (
 SELECT c.* FROM candidates c WHERE ($2::timestamptz IS NULL OR (c.started_at,c.attempt_id)<($2::timestamptz,$3::uuid))
 AND (NOT $4 OR c.status <> 'succeeded' OR NOT EXISTS (
 SELECT 1 FROM app.migration_history_entries r JOIN app.job_attempts ra ON ra.id=r.attempt_id AND ra.status='succeeded'
 WHERE r.database_id=c.database_id AND r.direction='down' AND r.is_complete AND r.reverts_execution_id IS NOT NULL
 AND (EXISTS (SELECT 1 FROM app.migration_history_entries src JOIN app.job_attempts sa ON sa.id=src.attempt_id AND sa.status='succeeded'
   WHERE src.database_id=r.database_id AND src.execution_id=r.reverts_execution_id AND src.is_complete AND src.direction='up')
   OR EXISTS (SELECT 1 FROM app.migration_executions e WHERE e.database_id=r.database_id AND e.id=r.reverts_execution_id
     AND e.status='succeeded' AND NOT EXISTS (SELECT 1 FROM app.jobs j WHERE j.database_id=e.database_id AND j.execution_id=e.id)))
 AND (r.attempt_id=c.attempt_id OR (r.reverts_execution_id=c.execution_id AND (
   EXISTS (SELECT 1 FROM app.migration_history_entries src WHERE src.database_id=c.database_id
     AND src.execution_id=c.execution_id AND src.attempt_id=c.attempt_id AND src.is_complete AND src.direction='up')
   OR (c.legacy_commit IS NOT NULL AND c.attempt_id=c.execution_id))))))
 ORDER BY c.started_at DESC,c.attempt_id DESC LIMIT $5
)
 SELECT p.attempt_id::text,p.started_at,head.active_head_execution_id::text,CASE WHEN p.attempt_id IS NOT NULL THEN jsonb_build_object(
 'attempt_id',p.attempt_id,'execution_id',p.execution_id,'migration_id',p.migration_id,
 'revision',p.revision,'version',p.version,'actor',jsonb_build_object('id',p.actor_id,'username',p.username),
 'action',p.action,'direction',p.direction,'status',p.status,'started_at',p.started_at,'finished_at',p.finished_at,
 'source_execution_id',p.source_execution_id,'sql_entered',p.sql_entered,
 'statements',COALESCE((SELECT jsonb_agg(jsonb_build_object('ordinal',s.ordinal,'batch_index',s.batch_index,
 'sql',s.sql_text,'parameters',s.safe_parameters,'outcome',s.outcome) ORDER BY s.ordinal)
 FROM app.job_statement_evidence s WHERE s.attempt_id=p.attempt_id),
 CASE WHEN p.legacy_sql IS NOT NULL AND p.legacy_sql <> '' THEN jsonb_build_array(jsonb_build_object(
 'ordinal',1,'batch_index',NULL,'sql',p.legacy_sql,'parameters','{}'::jsonb,'outcome','unknown')) END,'[]'::jsonb),
 'commit',COALESCE((` + commitProjection + ` AND b.attempt_id=p.attempt_id ` + commitProjectionEnd + ` WHERE h.database_id=p.database_id AND h.attempt_id=p.attempt_id ORDER BY h.version DESC LIMIT 1),p.legacy_commit),
 'is_active_head',COALESCE(p.status='succeeded' AND p.execution_id=head.active_head_execution_id AND (
   EXISTS (SELECT 1 FROM app.migration_history_entries h WHERE h.database_id=p.database_id
     AND h.execution_id=p.execution_id AND h.attempt_id=p.attempt_id AND h.is_complete AND h.direction='up')
   OR p.legacy_commit IS NOT NULL),false),
 'allowed_actions',COALESCE(actions.value,'[]'::jsonb)) END
 FROM (SELECT (SELECT active_head_execution_id FROM app.migration_active_heads WHERE database_id=$1) AS active_head_execution_id) head
 LEFT JOIN page p ON true
 LEFT JOIN LATERAL (
 SELECT to_jsonb(array_remove(ARRAY[
 CASE WHEN app.workflow_source_valid(j.database_id,d.execution_target_id,j.migration_id,j.revision,'rollback',p.execution_id,j.mode)
   THEN 'propose_rollback' END,
 CASE WHEN app.workflow_source_valid(j.database_id,d.execution_target_id,j.migration_id,j.revision,'forward',p.execution_id,j.mode)
   THEN 'propose_forward' END,
 CASE WHEN app.workflow_source_valid(j.database_id,d.execution_target_id,j.migration_id,j.revision,'compensate',p.execution_id,j.mode)
   AND ` + executioncontract.ReversibleSnapshotSQL("j") + `
   THEN 'propose_compensate' END],NULL)) AS value
 FROM app.jobs j
 JOIN app.database_connections d ON d.id=j.database_id AND d.deleted_at IS NULL
 JOIN app.migration_drafts draft ON draft.id=j.migration_id AND draft.database_id=j.database_id
   AND draft.revision=j.revision AND draft.mode=j.mode
 WHERE j.execution_id=p.execution_id AND j.database_id=p.database_id AND p.status='succeeded'
 AND EXISTS (SELECT 1 FROM app.migration_history_entries h WHERE h.database_id=p.database_id
   AND h.execution_id=p.execution_id AND h.attempt_id=p.attempt_id AND h.job_id=j.id AND h.is_complete AND h.direction='up')
 AND EXISTS (SELECT 1 FROM app.group_memberships m JOIN app.group_database_grants gr ON gr.group_id=m.group_id
   JOIN app.users u ON u.id=m.user_id AND u.is_active
   WHERE m.user_id=$6 AND gr.database_id=p.database_id AND gr.access='edit')
 AND NOT EXISTS (SELECT 1 FROM app.jobs blocker WHERE blocker.target_id=d.execution_target_id AND blocker.blocks_database)
 ORDER BY j.id LIMIT 1
 ) actions ON true
 ORDER BY p.started_at DESC,p.attempt_id DESC`

func (h *Handler) listAttempts(ctx context.Context, w http.ResponseWriter, r *http.Request, database, actor string) {
	q := r.URL.Query()
	limit := 20
	if q.Has("limit") {
		var err error
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > 100 {
			apiError(w, 400, "INVALID_REQUEST", "Invalid limit")
			return
		}
	}
	rawHide := q.Get("hide_rollback")
	if q.Has("hide_rollback") && rawHide != "true" && rawHide != "false" {
		apiError(w, 400, "INVALID_REQUEST", "Invalid hide_rollback")
		return
	}
	hide := rawHide == "true"
	var started any
	var attempt any
	if q.Has("cursor") {
		var c historyCursor
		data, err := base64.RawURLEncoding.DecodeString(q.Get("cursor"))
		if err != nil || json.Unmarshal(data, &c) != nil || c.Database != database || c.Hide != hide || c.Started.IsZero() || !validUUID(c.Attempt) {
			apiError(w, 400, "INVALID_REQUEST", "Invalid cursor")
			return
		}
		started, attempt = c.Started, c.Attempt
	}
	rows, err := h.d.Platform.QueryContext(ctx, attemptPageQuery, database, started, attempt, hide, limit+1, actor)
	if err != nil {
		internal(w)
		return
	}
	defer rows.Close()
	items := make([]json.RawMessage, 0, limit)
	var last historyCursor
	more := false
	var head sql.NullString
	for rows.Next() {
		var id sql.NullString
		var at sql.NullTime
		var item []byte
		if err := rows.Scan(&id, &at, &head, &item); err != nil {
			internal(w)
			return
		}
		if !id.Valid {
			continue
		}
		if len(items) == limit {
			more = true
			break
		}
		items = append(items, item)
		last = historyCursor{Database: database, Hide: hide, Started: at.Time, Attempt: id.String}
	}
	if rows.Err() != nil {
		internal(w)
		return
	}
	var next *string
	if more {
		data, err := json.Marshal(last)
		if err != nil {
			internal(w)
			return
		}
		cursor := base64.RawURLEncoding.EncodeToString(data)
		next = &cursor
	}
	var activeHead *string
	if head.Valid {
		activeHead = &head.String
	}
	write(w, http.StatusOK, map[string]any{"data": map[string]any{"items": items, "next_cursor": next, "active_head_execution_id": activeHead}})
}
