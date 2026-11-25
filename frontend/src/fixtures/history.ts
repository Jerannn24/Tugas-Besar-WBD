import type { TransportRequest } from '../api/client'
import type { AllowedAction, Execution, HistoryAttemptRow, ID, LegacyHistory } from '../api/types'
import { failM2, ok, okM2 } from './http'
import { listResponse, query } from './paging'
import type { AttemptRecord } from './samples'
import { notFound, type Ctx, type State } from './state'

type CommittedAttempt = AttemptRecord & { commit: NonNullable<AttemptRecord['commit']> }

export const byStartedDesc = (a: AttemptRecord, b: AttemptRecord) =>
  b.started_at.localeCompare(a.started_at) || b.attempt_id.localeCompare(a.attempt_id)

const isUpCommit = (a: AttemptRecord) =>
  a.status === 'succeeded' && a.commit !== null && a.commit.direction === 'up' && a.commit.is_complete

function revertedIds(rows: AttemptRecord[]): Set<ID> {
  const ids = new Set<ID>()
  for (const r of rows) {
    const reverts = r.commit?.reverts_execution_id
    if (r.status === 'succeeded' && r.commit?.direction === 'down' && r.commit.is_complete && reverts) ids.add(reverts)
  }
  return ids
}

function activeHead(rows: AttemptRecord[]): ID | null {
  const reverted = revertedIds(rows)
  return rows.filter(isUpCommit).sort(byStartedDesc).find((r) => !reverted.has(r.execution_id))?.execution_id ?? null
}

type RowContext = { head: ID | null; reverted: Set<ID>; edit: boolean; state: State }

function rowActions(a: AttemptRecord, rc: RowContext): AllowedAction[] {
  if (!rc.edit || !isUpCommit(a)) return []
  const draft = rc.state.drafts.find((d) => d.id === a.migration_id && d.database_id === a.database_id)
  if (!draft || draft.revision !== a.revision) return []
  const actions: AllowedAction[] = []
  if (a.execution_id === rc.head && draft.sql_down) actions.push('propose_rollback')
  if (rc.reverted.has(a.execution_id) && rc.head !== a.execution_id) actions.push('propose_forward')
  return actions
}

function historyRow(a: AttemptRecord, rc: RowContext): HistoryAttemptRow {
  return {
    attempt_id: a.attempt_id,
    execution_id: a.execution_id,
    migration_id: a.migration_id,
    revision: a.revision,
    version: a.version,
    actor: a.actor,
    action: a.action,
    direction: a.direction,
    status: a.status,
    started_at: a.started_at,
    finished_at: a.finished_at,
    source_execution_id: a.source_execution_id,
    sql_entered: a.sql_entered,
    statements: a.statements,
    commit: a.commit,
    is_active_head: isUpCommit(a) && a.execution_id === rc.head,
    allowed_actions: rowActions(a, rc),
  }
}

function legacyView(a: CommittedAttempt): LegacyHistory {
  const c = a.commit
  return {
    version: c.version,
    executed_by: c.executed_by,
    id: c.id,
    database_id: c.database_id,
    draft_id: c.draft_id,
    draft_revision: c.draft_revision,
    title: c.title,
    sql: c.sql,
    started_at: c.started_at,
    finished_at: c.finished_at,
    status: c.status,
    error: c.error,
  }
}

export function hasHistoryQuery(req: TransportRequest): boolean {
  const q = query(req)
  return q.has('limit') || q.has('cursor') || q.has('hide_rollback')
}

function readHide(req: TransportRequest): boolean | Response {
  const raw = query(req).get('hide_rollback')
  if (raw === null || raw === 'false') return false
  if (raw === 'true') return true
  return failM2(400, 'INVALID_REQUEST', 'Parameter hide_rollback tidak valid.', { hide_rollback: 'Harus true atau false.' })
}

function hideRollback(rows: AttemptRecord[]): AttemptRecord[] {
  const hidden = new Set<ID>()
  for (const r of rows) {
    const reverts = r.commit?.reverts_execution_id ?? null
    if (r.action === 'rollback' && r.status === 'succeeded' && reverts) {
      hidden.add(reverts)
      hidden.add(r.execution_id)
    }
  }
  return rows.filter((r) => !(r.status === 'succeeded' && hidden.has(r.execution_id)))
}

export function legacyAttemptPage(req: TransportRequest, databaseId: ID, executions: Execution[]): Response {
  const hide = readHide(req)
  if (hide instanceof Response) return hide
  const rows: HistoryAttemptRow[] = executions
    .slice()
    .sort((a, b) => b.started_at.localeCompare(a.started_at) || b.id.localeCompare(a.id))
    .map((x) => ({
      attempt_id: x.id,
      execution_id: x.id,
      migration_id: x.draft_id,
      revision: x.draft_revision,
      version: x.status === 'succeeded' ? x.version : null,
      actor: x.executed_by,
      action: 'apply',
      direction: 'up',
      status: x.status,
      started_at: x.started_at,
      finished_at: x.finished_at,
      source_execution_id: null,
      sql_entered: null,
      statements: [],
      is_active_head: false,
      allowed_actions: [],
      commit:
        x.status === 'succeeded'
          ? {
              execution_id: x.id,
              migration_id: x.draft_id,
              revision: x.draft_revision,
              version: x.version,
              action: 'apply',
              direction: 'up',
              executed_by: x.executed_by,
              committed_at: x.finished_at ?? x.started_at,
              sql_snapshot: x.sql,
              source_execution_id: null,
              reverts_execution_id: null,
              is_complete: true,
              batch_commits: [],
              id: x.id,
              database_id: x.database_id,
              draft_id: x.draft_id,
              draft_revision: x.draft_revision,
              title: x.title,
              sql: x.sql,
              started_at: x.started_at,
              finished_at: x.finished_at,
              status: x.status,
              error: x.error,
            }
          : null,
    }))
  return listResponse(req, { db: databaseId, resource: 'migrations', hide }, rows, { active_head_execution_id: null })
}

function listHistory(c: Ctx): Response {
  const rows = c.state.attempts.filter((a) => a.database_id === c.databaseId).sort(byStartedDesc)
  if (!hasHistoryQuery(c.req)) {
    return ok({ items: rows.filter((a): a is CommittedAttempt => a.commit !== null).map(legacyView) })
  }
  const hide = readHide(c.req)
  if (hide instanceof Response) return hide
  const visible = hide ? hideRollback(rows) : rows
  const rc: RowContext = { head: activeHead(rows), reverted: revertedIds(rows), edit: c.access === 'edit', state: c.state }
  return listResponse(
    c.req,
    { db: c.databaseId, resource: 'migrations', hide },
    visible.map((a) => historyRow(a, rc)),
    { active_head_execution_id: rc.head },
  )
}

export function historyRoute(key: string, c: Ctx): Response | null {
  switch (key) {
    case 'GET migrations':
      return listHistory(c)
    case 'GET migrations/:id': {
      const a = c.state.attempts.find((x) => x.commit?.execution_id === c.id && x.database_id === c.databaseId)
      return a?.commit ? okM2(a.commit) : notFound('Execution')
    }
  }
  return null
}
