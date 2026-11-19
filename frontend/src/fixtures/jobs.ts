import type { AccessLevel, Attempt, JobAccepted, JobView, MigrationAction } from '../api/types'
import { USERS } from './data'
import { okM2 } from './http'
import { byStartedDesc } from './history'
import { listResponse } from './paging'
import { commitEntry, type AttemptRecord } from './samples'
import { iso, nextId, notFound, type Ctx, type State, type StoredJob } from './state'

const QUEUED_MS = 2_000
const RUNNING_MS = 6_000

export function acceptedView(j: JobView): JobAccepted {
  return {
    job_id: j.job_id,
    execution_id: j.execution_id,
    proposal_id: j.proposal_id,
    database_id: j.database_id,
    status: j.status,
    status_url: j.status_url,
  }
}

function jobView(j: StoredJob, access: AccessLevel): JobView {
  return {
    ...acceptedView(j),
    migration_id: j.migration_id,
    revision: j.revision,
    action: j.action,
    mode: j.mode,
    execution_status: j.execution_status,
    attempt_no: j.attempt_no,
    blocks_database: j.blocks_database,
    progress: j.progress,
    source_execution_id: j.source_execution_id,
    created_at: j.created_at,
    started_at: j.started_at,
    finished_at: j.finished_at,
    next_retry_at: j.next_retry_at,
    error: j.error,
    allowed_actions: access === 'edit' ? j.allowed_actions : [],
  }
}

function attemptView(a: AttemptRecord): Attempt {
  return {
    attempt_id: a.attempt_id,
    job_id: a.job_id,
    execution_id: a.execution_id,
    database_id: a.database_id,
    migration_id: a.migration_id,
    revision: a.revision,
    actor_id: a.actor_id,
    attempt_no: a.attempt_no,
    status: a.status,
    started_at: a.started_at,
    finished_at: a.finished_at,
    error: a.error,
    legacy_execution_id: a.legacy_execution_id,
  }
}

const directionOf = (action: MigrationAction) => (action === 'rollback' || action === 'compensate' ? 'down' : 'up')

export function settle(state: State): void {
  const now = Date.now()
  for (const j of state.jobs) {
    if (j.sim_t0 === undefined) continue
    if (j.status === 'queued' && now >= j.sim_t0 + QUEUED_MS) startJob(state, j, j.sim_t0 + QUEUED_MS)
    if (j.status === 'running' && now >= j.sim_t0 + QUEUED_MS + RUNNING_MS) {
      finishJob(state, j, j.sim_t0 + QUEUED_MS + RUNNING_MS)
      delete j.sim_t0
      delete j.sim_actor
    }
  }
}

function startJob(state: State, j: StoredJob, at: number): void {
  const actor = j.sim_actor ?? null
  j.status = 'running'
  j.execution_status = 'running'
  j.started_at = iso(at)
  j.blocks_database = true
  if (j.mode === 'batch') {
    j.progress = { direction: directionOf(j.action), batches_committed: '0', rows_committed: '0', batches_reverted: '0', rows_reverted: '0', total_rows: null }
  }
  state.attempts.push({
    attempt_id: nextId(state, '80000900'),
    job_id: j.job_id,
    execution_id: j.execution_id,
    database_id: j.database_id,
    migration_id: j.migration_id,
    revision: j.revision,
    version: null,
    actor,
    actor_id: actor?.id ?? USERS.operator.id,
    action: j.action,
    direction: directionOf(j.action),
    status: 'running',
    attempt_no: j.attempt_no,
    started_at: iso(at),
    finished_at: null,
    source_execution_id: j.source_execution_id,
    sql_entered: null,
    statements: [],
    commit: null,
    error: null,
    legacy_execution_id: null,
  })
}

function finishJob(state: State, j: StoredJob, at: number): void {
  const direction = directionOf(j.action)
  const proposal = state.proposals.find((p) => p.proposal_id === j.proposal_id)
  const sql = (direction === 'down' ? proposal?.snapshot.sql_down : proposal?.snapshot.sql_up) ?? ''
  const version = String(state.next_version++)

  j.status = 'succeeded'
  j.execution_status = 'succeeded'
  j.finished_at = iso(at)
  j.blocks_database = false
  if (j.mode === 'batch') {
    j.progress =
      direction === 'up'
        ? { direction, batches_committed: '1', rows_committed: '42', batches_reverted: '0', rows_reverted: '0', total_rows: null }
        : { direction, batches_committed: '1', rows_committed: '42', batches_reverted: '1', rows_reverted: '42', total_rows: null }
  }
  j.allowed_actions = j.action === 'apply' || j.action === 'forward' ? ['propose_rollback'] : []

  // Resume: job sumber dengan execution yang sama membaca execution_status terbaru.
  for (const other of state.jobs) {
    if (other !== j && other.execution_id === j.execution_id) {
      other.execution_status = 'succeeded'
      other.blocks_database = false
      other.allowed_actions = []
    }
  }

  const a = state.attempts.find((x) => x.job_id === j.job_id && x.status === 'running')
  if (a) {
    a.status = 'succeeded'
    a.finished_at = iso(at)
    a.version = version
    a.sql_entered = true
    a.statements = [{ ordinal: 1, batch_index: j.mode === 'batch' ? 1 : null, sql, parameters: {}, outcome: 'succeeded' }]
    a.commit = commitEntry(
      {
        execution_id: j.execution_id,
        migration_id: j.migration_id,
        version,
        action: j.action,
        direction,
        sql,
        started_at: j.started_at ?? iso(at),
        committed_at: iso(at),
        source_execution_id: j.source_execution_id,
        reverts_execution_id: j.action === 'rollback' ? j.source_execution_id : null,
      },
      a.actor ?? undefined,
    )
  }

  const draft = state.drafts.find((d) => d.id === j.migration_id)
  if (draft) {
    if (j.action === 'rollback') draft.applied_execution_id = null
    else if (j.action !== 'compensate') draft.applied_execution_id = j.execution_id
  }
}

export function jobsRoute(key: string, c: Ctx): Response | null {
  const { state, databaseId, req } = c
  switch (key) {
    case 'GET jobs':
      return listResponse(
        req,
        { db: databaseId, resource: 'jobs', hide: false },
        state.jobs
          .filter((j) => j.database_id === databaseId)
          .sort((a, b) => b.created_at.localeCompare(a.created_at) || b.job_id.localeCompare(a.job_id))
          .map((j) => jobView(j, c.access)),
      )
    case 'GET jobs/:id': {
      const j = state.jobs.find((x) => x.job_id === c.id && x.database_id === databaseId)
      return j ? okM2(jobView(j, c.access)) : notFound('Job')
    }
    case 'GET attempts':
      return listResponse(
        req,
        { db: databaseId, resource: 'attempts', hide: false },
        state.attempts
          .filter((a) => a.database_id === databaseId)
          .sort(byStartedDesc)
          .map(attemptView),
      )
  }
  return null
}
