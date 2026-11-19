import type {
  Actor,
  Approval,
  BatchCommit,
  BatchConfig,
  Draft,
  HistoryEntry,
  HistoryStatement,
  ID,
  JobError,
  JobStatus,
  JobView,
  MigrationAction,
  Direction,
  Progress,
  Proposal,
  ProposalStatus,
  Snapshot,
  Validation,
} from '../api/types'
import { TARGETS, USERS } from './data'

export const M2_DB: ID = TARGETS.demo_m2.id

export const OPERATOR: Actor = { id: USERS.operator.id, username: USERS.operator.username }


export const GROUP_MAIN: ID = '00000030-0000-4000-8000-000000000030'
export const GROUP_OPS: ID = '00000031-0000-4000-8000-000000000031'
export const GROUP_EDITORS: ID = '00000032-0000-4000-8000-000000000032'
export const GROUP_NO_LEADER: ID = '50000000-0000-4000-8000-000000000001'

export const REQUIRED_GROUPS: { group_id: ID; leader_user_id: ID | null }[] = [
  { group_id: GROUP_MAIN, leader_user_id: OPERATOR.id },
]

export const MIG_A: ID = '00000010-0000-4000-8000-000000000010'
export const MIG_B: ID = '00000011-0000-4000-8000-000000000011'
export const MIG_C: ID = '00000012-0000-4000-8000-000000000012'
export const MIG_REJECTED: ID = '00000013-0000-4000-8000-000000000013'
export const MIG_STD: ID = '30000000-0000-4000-8000-000000000099'

const T0 = '2026-01-01T00:00:00Z'
const T5 = '2026-01-01T00:05:00Z'

const SQL_A_UP = 'UPDATE managed.batch_items SET processed = TRUE WHERE id = ANY($1::bigint[]);'
const SQL_A_DOWN = 'UPDATE managed.batch_items SET processed = FALSE WHERE id = ANY($1::bigint[]);'
const SQL_B_UP = 'ALTER TABLE managed.batch_items ADD COLUMN note text;'
const SQL_B_DOWN = 'ALTER TABLE managed.batch_items DROP COLUMN note;'
const SQL_C_UP = 'CREATE INDEX batch_items_processed_idx ON managed.batch_items (processed);'
const SQL_C_DOWN = 'DROP INDEX managed.batch_items_processed_idx;'
const SQL_STD_UP = "UPDATE managed.batch_items SET processed = TRUE WHERE id = 1;"

const BATCH_A: BatchConfig = {
  schema: 'managed',
  table: 'batch_items',
  cursor_column: 'id',
  batch_size: 100,
  parameters: {},
}

const IDS_1_42 = Array.from({ length: 42 }, (_, i) => String(i + 1))

export function seedDrafts(): Draft[] {
  const base = { database_id: M2_DB, revision: 1, created_by: OPERATOR.id, updated_at: T0 }
  return [
    {
      ...base,
      id: MIG_A,
      title: 'Add example',
      sql: SQL_A_UP,
      applied_execution_id: '00000401-0000-4000-8000-000000000401',
      sql_down: SQL_A_DOWN,
      irreversible_reason: null,
      mode: 'batch',
      batch: BATCH_A,
    },
    {
      ...base,
      id: MIG_B,
      title: 'Tambah kolom note',
      sql: SQL_B_UP,
      applied_execution_id: '00000404-0000-4000-8000-000000000404',
      sql_down: SQL_B_DOWN,
      irreversible_reason: null,
      mode: 'standard',
      batch: null,
    },
    {
      ...base,
      id: MIG_C,
      title: 'Tambah indeks processed',
      sql: SQL_C_UP,
      applied_execution_id: '00000405-0000-4000-8000-000000000405',
      sql_down: SQL_C_DOWN,
      irreversible_reason: null,
      mode: 'standard',
      batch: null,
    },
    {
      ...base,
      id: MIG_STD,
      title: 'Standard example',
      sql: SQL_STD_UP,
      applied_execution_id: '60000000-0000-4000-8000-000000000099',
      sql_down: null,
      irreversible_reason: 'Nilai lama tidak disimpan.',
      mode: 'standard',
      batch: null,
    },
    {
      ...base,
      id: MIG_REJECTED,
      title: 'Contoh SQL ditolak validasi (fixture)',
      sql: 'BEGIN;\nUPDATE managed.batch_items SET processed = TRUE;\nCOMMIT;',
      applied_execution_id: null,
      sql_down: 'UPDATE managed.batch_items SET processed = FALSE;',
      irreversible_reason: null,
      mode: 'standard',
      batch: null,
    },
  ]
}

export function snapshotOf(d: Draft): Snapshot {
  return {
    migration_id: d.id,
    database_id: d.database_id,
    title: d.title,
    sql_up: d.sql,
    sql_down: d.sql_down ?? null,
    irreversible_reason: d.irreversible_reason ?? null,
    revision: d.revision,
    mode: d.mode ?? 'standard',
    batch: d.batch ?? null,
  }
}

const DRAFTS = new Map(seedDrafts().map((d) => [d.id, d]))
const snap = (migrationId: ID): Snapshot => snapshotOf(DRAFTS.get(migrationId)!)

// Proposal

const PASSED: Validation = { status: 'passed', checked_at: T0, issues: [] }
const pending = (group_id: ID, leader_user_id: ID | null): Approval => ({
  group_id,
  leader_user_id,
  status: 'pending',
  approved_at: null,
})
const approved = (group_id: ID, leader_user_id: ID | null): Approval => ({
  group_id,
  leader_user_id,
  status: 'approved',
  approved_at: T0,
})

function proposal(
  proposal_id: ID,
  migration_id: ID,
  status: ProposalStatus,
  extra: Partial<Proposal> = {},
): Proposal {
  return {
    proposal_id,
    database_id: M2_DB,
    migration_id,
    revision: 1,
    action: 'apply',
    source_execution_id: null,
    status,
    snapshot: snap(migration_id),
    validation: PASSED,
    approvals: [],
    allowed_actions: [],
    ...extra,
  }
}

export const P = {
  draft: '45000000-0000-4000-8000-000000000001',
  pending: '00000100-0000-4000-8000-000000000100',
  approved: '00000101-0000-4000-8000-000000000101',
  superseded: '00000102-0000-4000-8000-000000000102',
  noLeader: '00000103-0000-4000-8000-000000000103',
  multiLeader: '00000104-0000-4000-8000-000000000104',
  queueDown: '00000105-0000-4000-8000-000000000105',
  rejected: '00000106-0000-4000-8000-000000000106',
  leaderChanged: '00000107-0000-4000-8000-000000000107',
  dropOnce: '00000108-0000-4000-8000-000000000108',
} as const

export const QUEUE_DOWN_ONCE: ID[] = [P.queueDown]
export const LEADER_SWAP_ONCE: ID[] = [P.leaderChanged]
export const RESPONSE_DROP_ONCE: ID[] = [P.dropOnce]

const statusUrl = (jobId: ID) => `/api/databases/${M2_DB}/jobs/${jobId}`

const progress = (
  direction: Direction,
  batches_committed: string,
  rows_committed: string,
  batches_reverted = '0',
  rows_reverted = '0',
): Progress => ({ direction, batches_committed, rows_committed, batches_reverted, rows_reverted, total_rows: null })

const UP_1_42 = progress('up', '1', '42')

type JobSeed = {
  job_id: ID
  execution_id: ID
  proposal_id: ID
  migration_id?: ID
  status: JobStatus
  execution_status?: JobStatus
  action?: MigrationAction
  mode?: 'standard' | 'batch'
  attempt_no?: number
  blocks_database: boolean
  progress: Progress | null
  source_execution_id?: ID | null
  created_at?: string
  started_at?: string | null
  finished_at?: string | null
  next_retry_at?: string | null
  error?: JobError | null
  allowed_actions?: JobView['allowed_actions']
}

function job(s: JobSeed): JobView {
  return {
    job_id: s.job_id,
    execution_id: s.execution_id,
    proposal_id: s.proposal_id,
    database_id: M2_DB,
    status: s.status,
    status_url: statusUrl(s.job_id),
    migration_id: s.migration_id ?? MIG_A,
    revision: 1,
    action: s.action ?? 'apply',
    mode: s.mode ?? 'batch',
    execution_status: s.execution_status ?? s.status,
    attempt_no: s.attempt_no ?? 1,
    blocks_database: s.blocks_database,
    progress: s.progress,
    source_execution_id: s.source_execution_id ?? null,
    created_at: s.created_at ?? T0,
    started_at: s.started_at === undefined ? T0 : s.started_at,
    finished_at: s.finished_at === undefined ? T5 : s.finished_at,
    next_retry_at: s.next_retry_at ?? null,
    error: s.error ?? null,
    allowed_actions: s.allowed_actions ?? [],
  }
}

export const REVIEW_STALE_ERROR: JobError = {
  code: 'REVIEW_STALE',
  message: 'Approval sudah usang sebelum SQL dijalankan. Proposal kembali ke Pending Review.',
  retryable: false,
}

const SQL_FAILED_RETRYABLE: JobError = {
  code: 'SQL_FAILED',
  message: 'Eksekusi SQL gagal',
  retryable: true,
}

const SQL_FAILED_FINAL: JobError = {
  code: 'SQL_FAILED',
  message: 'Eksekusi SQL gagal',
  retryable: false,
}

export function seedJobs(): JobView[] {
  return [
    job({
      job_id: '00000200-0000-4000-8000-000000000200',
      execution_id: '00000300-0000-4000-8000-000000000300',
      proposal_id: '00000110-0000-4000-8000-000000000110',
      status: 'queued',
      blocks_database: false,
      progress: null,
      started_at: null,
      finished_at: null,
    }),
    job({
      job_id: '00000201-0000-4000-8000-000000000201',
      execution_id: '00000301-0000-4000-8000-000000000301',
      proposal_id: '00000111-0000-4000-8000-000000000111',
      status: 'running',
      blocks_database: true,
      progress: UP_1_42,
      finished_at: null,
    }),
    job({
      job_id: '00000202-0000-4000-8000-000000000202',
      execution_id: '00000401-0000-4000-8000-000000000401',
      proposal_id: '00000112-0000-4000-8000-000000000112',
      status: 'succeeded',
      attempt_no: 2,
      blocks_database: false,
      progress: UP_1_42,
      allowed_actions: ['propose_rollback'],
    }),
    job({
      job_id: '00000203-0000-4000-8000-000000000203',
      execution_id: '00000303-0000-4000-8000-000000000303',
      proposal_id: '00000113-0000-4000-8000-000000000113',
      status: 'failed',
      blocks_database: false,
      progress: null,
      error: REVIEW_STALE_ERROR,
    }),
    job({
      job_id: '00000204-0000-4000-8000-000000000204',
      execution_id: '00000304-0000-4000-8000-000000000304',
      proposal_id: '00000114-0000-4000-8000-000000000114',
      status: 'retry_wait',
      attempt_no: 2,
      blocks_database: true,
      progress: progress('up', '0', '0'),
      finished_at: null,
      next_retry_at: T5,
    }),
    job({
      job_id: '00000205-0000-4000-8000-000000000205',
      execution_id: '00000305-0000-4000-8000-000000000305',
      proposal_id: '00000115-0000-4000-8000-000000000115',
      status: 'partial',
      blocks_database: true,
      progress: UP_1_42,
      allowed_actions: ['propose_resume', 'propose_compensate'],
    }),
    job({
      job_id: '00000206-0000-4000-8000-000000000206',
      execution_id: '00000306-0000-4000-8000-000000000306',
      proposal_id: '00000116-0000-4000-8000-000000000116',
      status: 'partially_reverted',
      action: 'compensate',
      blocks_database: true,
      progress: progress('down', '1', '42', '1', '10'),
      source_execution_id: '00000305-0000-4000-8000-000000000305',
      allowed_actions: ['propose_resume'],
    }),
    job({
      job_id: '00000207-0000-4000-8000-000000000207',
      execution_id: '00000307-0000-4000-8000-000000000307',
      proposal_id: '00000117-0000-4000-8000-000000000117',
      status: 'unknown',
      blocks_database: true,
      progress: UP_1_42,
      finished_at: null,
    }),
    job({
      job_id: '00000208-0000-4000-8000-000000000208',
      execution_id: '00000308-0000-4000-8000-000000000308',
      proposal_id: '00000118-0000-4000-8000-000000000118',
      status: 'partial',
      execution_status: 'succeeded',
      blocks_database: false,
      progress: UP_1_42,
    }),
    job({
      job_id: '00000209-0000-4000-8000-000000000209',
      execution_id: '00000308-0000-4000-8000-000000000308',
      proposal_id: '00000119-0000-4000-8000-000000000119',
      status: 'succeeded',
      action: 'resume',
      blocks_database: false,
      progress: UP_1_42,
      source_execution_id: '00000308-0000-4000-8000-000000000308',
    }),
    job({
      job_id: '50000000-0000-4000-8000-000000000099',
      execution_id: '60000000-0000-4000-8000-000000000099',
      proposal_id: '40000000-0000-4000-8000-000000000099',
      migration_id: MIG_STD,
      status: 'succeeded',
      mode: 'standard',
      blocks_database: false,
      progress: null,
    }),
    job({
      job_id: '00000210-0000-4000-8000-000000000210',
      execution_id: E.e2,
      proposal_id: '00000120-0000-4000-8000-000000000120',
      migration_id: MIG_B,
      status: 'succeeded',
      mode: 'standard',
      blocks_database: false,
      progress: null,
      created_at: '2026-01-02T00:00:00Z',
      started_at: '2026-01-02T00:00:00Z',
      finished_at: '2026-01-02T00:00:01Z',
    }),
    job({
      job_id: '00000211-0000-4000-8000-000000000211',
      execution_id: E.r1,
      proposal_id: '00000121-0000-4000-8000-000000000121',
      migration_id: MIG_B,
      status: 'failed',
      action: 'rollback',
      mode: 'standard',
      blocks_database: false,
      progress: null,
      source_execution_id: E.e2,
      created_at: '2026-01-03T00:00:00Z',
      started_at: '2026-01-03T00:00:00Z',
      finished_at: '2026-01-03T00:00:01Z',
      error: SQL_FAILED_FINAL,
    }),
    job({
      job_id: '00000212-0000-4000-8000-000000000212',
      execution_id: E.e3,
      proposal_id: '00000122-0000-4000-8000-000000000122',
      migration_id: MIG_B,
      status: 'succeeded',
      action: 'rollback',
      mode: 'standard',
      blocks_database: false,
      progress: null,
      source_execution_id: E.e2,
      created_at: '2026-01-03T01:00:00Z',
      started_at: '2026-01-03T01:00:00Z',
      finished_at: '2026-01-03T01:00:01Z',
    }),
    job({
      job_id: '00000213-0000-4000-8000-000000000213',
      execution_id: E.e4,
      proposal_id: '00000123-0000-4000-8000-000000000123',
      migration_id: MIG_B,
      status: 'succeeded',
      action: 'forward',
      mode: 'standard',
      blocks_database: false,
      progress: null,
      source_execution_id: E.e2,
      created_at: '2026-01-04T00:00:00Z',
      started_at: '2026-01-04T00:00:00Z',
      finished_at: '2026-01-04T00:00:01Z',
      allowed_actions: ['propose_rollback'],
    }),
    job({
      job_id: '00000214-0000-4000-8000-000000000214',
      execution_id: E.e5,
      proposal_id: '00000124-0000-4000-8000-000000000124',
      migration_id: MIG_C,
      status: 'succeeded',
      mode: 'standard',
      blocks_database: false,
      progress: null,
      created_at: '2026-01-05T00:00:00Z',
      started_at: '2026-01-05T00:00:00Z',
      finished_at: '2026-01-05T00:00:01Z',
      allowed_actions: ['propose_rollback'],
    }),
  ]
}

export function seedProposals(): Proposal[] {
  const explicit: Proposal[] = [
    proposal(P.draft, MIG_A, 'draft', { validation: null, allowed_actions: ['submit_review'] }),
    proposal(P.pending, MIG_A, 'pending_review', {
      approvals: [pending(GROUP_MAIN, OPERATOR.id)],
      allowed_actions: ['approve'],
    }),
    proposal(P.approved, MIG_A, 'approved', {
      approvals: [approved(GROUP_MAIN, OPERATOR.id)],
      allowed_actions: ['enqueue'],
    }),
    proposal(P.superseded, MIG_A, 'superseded', { approvals: [pending(GROUP_MAIN, OPERATOR.id)] }),
    proposal(P.noLeader, MIG_A, 'pending_review', {
      approvals: [pending(GROUP_MAIN, OPERATOR.id), pending(GROUP_NO_LEADER, null)],
      allowed_actions: ['approve'],
    }),
    proposal(P.multiLeader, MIG_A, 'pending_review', {
      approvals: [
        pending(GROUP_MAIN, OPERATOR.id),
        pending(GROUP_OPS, OPERATOR.id),
        pending(GROUP_EDITORS, USERS.demo_editor.id),
      ],
      allowed_actions: ['approve'],
    }),
    proposal(P.queueDown, MIG_A, 'approved', {
      approvals: [approved(GROUP_MAIN, OPERATOR.id)],
      allowed_actions: ['enqueue'],
    }),
    proposal(P.rejected, MIG_REJECTED, 'draft', { validation: null, allowed_actions: ['submit_review'] }),
    proposal(P.leaderChanged, MIG_A, 'pending_review', {
      approvals: [pending(GROUP_MAIN, OPERATOR.id)],
      allowed_actions: ['approve'],
    }),
    proposal(P.dropOnce, MIG_A, 'approved', {
      approvals: [approved(GROUP_MAIN, OPERATOR.id)],
      allowed_actions: ['enqueue'],
    }),
  ]

  const fromJobs = seedJobs().map((j) =>
    j.error?.code === 'REVIEW_STALE'
      ? proposal(j.proposal_id, j.migration_id, 'pending_review', {
          approvals: [pending(GROUP_MAIN, OPERATOR.id)],
          allowed_actions: ['approve'],
        })
      : proposal(j.proposal_id, j.migration_id, 'queued', {
          action: j.action,
          source_execution_id: j.action === 'apply' ? null : j.source_execution_id,
          approvals: [approved(GROUP_MAIN, OPERATOR.id)],
        }),
  )
  return [...explicit, ...fromJobs]
}

export const E = {
  e1: '00000401-0000-4000-8000-000000000401',
  e2: '00000402-0000-4000-8000-000000000402',
  e3: '00000403-0000-4000-8000-000000000403',
  e4: '00000404-0000-4000-8000-000000000404',
  e5: '00000405-0000-4000-8000-000000000405',
  r1: '00000406-0000-4000-8000-000000000406',
} as const

export type AttemptRecord = {
  attempt_id: ID
  job_id: ID | null
  execution_id: ID
  database_id: ID
  migration_id: ID
  revision: number
  version: string | null
  actor: Actor | null
  actor_id: ID
  action: MigrationAction
  direction: Direction
  status: string
  attempt_no: number
  started_at: string
  finished_at: string | null
  source_execution_id: ID | null
  sql_entered: boolean | null
  statements: HistoryStatement[]
  commit: HistoryEntry | null
  error: JobError | null
  legacy_execution_id: ID | null
}

const stmt = (
  sql: string,
  outcome: HistoryStatement['outcome'],
  batch_index: number | null = null,
  parameters: HistoryStatement['parameters'] = {},
): HistoryStatement => ({ ordinal: 1, batch_index, sql, parameters, outcome })

type CommitSeed = {
  execution_id: ID
  migration_id: ID
  version: string
  action: MigrationAction
  direction: Direction
  sql: string
  started_at: string
  committed_at: string
  source_execution_id?: ID | null
  reverts_execution_id?: ID | null
  batch_commits?: BatchCommit[]
}

export function commitEntry(c: CommitSeed, actor: Actor = OPERATOR): HistoryEntry {
  const draft = DRAFTS.get(c.migration_id)
  return {
    execution_id: c.execution_id,
    migration_id: c.migration_id,
    revision: 1,
    version: c.version,
    action: c.action,
    direction: c.direction,
    executed_by: actor,
    committed_at: c.committed_at,
    sql_snapshot: c.sql,
    source_execution_id: c.source_execution_id ?? null,
    reverts_execution_id: c.reverts_execution_id ?? null,
    is_complete: true,
    batch_commits: c.batch_commits ?? [],
    id: c.execution_id,
    database_id: M2_DB,
    draft_id: c.migration_id,
    draft_revision: 1,
    title: draft?.title ?? 'Migrasi',
    sql: c.sql,
    started_at: c.started_at,
    finished_at: c.committed_at,
    status: 'succeeded',
    error: null,
  }
}

export const E1_COMMIT: HistoryEntry = {
  execution_id: E.e1,
  migration_id: MIG_A,
  revision: 1,
  version: '1',
  action: 'apply',
  direction: 'up',
  executed_by: OPERATOR,
  committed_at: T5,
  sql_snapshot: SQL_A_UP,
  source_execution_id: null,
  reverts_execution_id: null,
  is_complete: true,
  batch_commits: [
    {
      batch_index: '1',
      row_count: '42',
      committed_at: T5,
      receipt: 'receipt-example',
      range_start: '1',
      range_end: '42',
      sql_snapshot: SQL_A_UP,
      safe_parameters: { ids: IDS_1_42 },
      job_id: '00000202-0000-4000-8000-000000000202',
      executed_by: OPERATOR,
    },
  ],
  id: E.e1,
  database_id: M2_DB,
  draft_id: MIG_A,
  draft_revision: 1,
  title: 'Migrasi',
  sql: SQL_A_UP,
  started_at: T5,
  finished_at: T5,
  status: 'succeeded',
  error: null,
}

const attemptId = (n: string) => `80000000-0000-4000-8000-000000000${n}`

function attempt(
  a: Omit<AttemptRecord, 'database_id' | 'actor' | 'actor_id' | 'revision' | 'legacy_execution_id'> &
    Partial<Pick<AttemptRecord, 'revision' | 'legacy_execution_id' | 'actor'>>,
): AttemptRecord {
  return {
    database_id: M2_DB,
    actor: OPERATOR,
    actor_id: OPERATOR.id,
    revision: 1,
    legacy_execution_id: null,
    ...a,
  }
}

export function seedAttempts(): AttemptRecord[] {
  const batchParams = { ids: IDS_1_42 }
  return [
    attempt({
      attempt_id: attemptId('411'),
      job_id: '00000202-0000-4000-8000-000000000202',
      execution_id: E.e1,
      migration_id: MIG_A,
      version: null,
      action: 'apply',
      direction: 'up',
      status: 'failed',
      attempt_no: 1,
      started_at: T0,
      finished_at: '2026-01-01T00:00:30Z',
      source_execution_id: null,
      sql_entered: true,
      statements: [stmt(SQL_A_UP, 'failed', 1, batchParams)],
      commit: null,
      error: SQL_FAILED_RETRYABLE,
    }),
    attempt({
      attempt_id: attemptId('412'),
      job_id: '00000202-0000-4000-8000-000000000202',
      execution_id: E.e1,
      migration_id: MIG_A,
      version: '1',
      action: 'apply',
      direction: 'up',
      status: 'succeeded',
      attempt_no: 2,
      started_at: '2026-01-01T00:04:00Z',
      finished_at: T5,
      source_execution_id: null,
      sql_entered: true,
      statements: [stmt(SQL_A_UP, 'succeeded', 1, batchParams)],
      commit: E1_COMMIT,
      error: null,
    }),
    attempt({
      attempt_id: attemptId('420'),
      job_id: '00000210-0000-4000-8000-000000000210',
      execution_id: E.e2,
      migration_id: MIG_B,
      version: '2',
      action: 'apply',
      direction: 'up',
      status: 'succeeded',
      attempt_no: 1,
      started_at: '2026-01-02T00:00:00Z',
      finished_at: '2026-01-02T00:00:01Z',
      source_execution_id: null,
      sql_entered: true,
      statements: [stmt(SQL_B_UP, 'succeeded')],
      commit: commitEntry({
        execution_id: E.e2,
        migration_id: MIG_B,
        version: '2',
        action: 'apply',
        direction: 'up',
        sql: SQL_B_UP,
        started_at: '2026-01-02T00:00:00Z',
        committed_at: '2026-01-02T00:00:01Z',
      }),
      error: null,
    }),
    attempt({
      attempt_id: attemptId('460'),
      job_id: '00000211-0000-4000-8000-000000000211',
      execution_id: E.r1,
      migration_id: MIG_B,
      version: null,
      action: 'rollback',
      direction: 'down',
      status: 'failed',
      attempt_no: 1,
      started_at: '2026-01-03T00:00:00Z',
      finished_at: '2026-01-03T00:00:01Z',
      source_execution_id: E.e2,
      sql_entered: true,
      statements: [stmt(SQL_B_DOWN, 'failed')],
      commit: null,
      error: SQL_FAILED_FINAL,
    }),
    attempt({
      attempt_id: attemptId('430'),
      job_id: '00000212-0000-4000-8000-000000000212',
      execution_id: E.e3,
      migration_id: MIG_B,
      version: '3',
      action: 'rollback',
      direction: 'down',
      status: 'succeeded',
      attempt_no: 1,
      started_at: '2026-01-03T01:00:00Z',
      finished_at: '2026-01-03T01:00:01Z',
      source_execution_id: E.e2,
      sql_entered: true,
      statements: [stmt(SQL_B_DOWN, 'succeeded')],
      commit: commitEntry({
        execution_id: E.e3,
        migration_id: MIG_B,
        version: '3',
        action: 'rollback',
        direction: 'down',
        sql: SQL_B_DOWN,
        started_at: '2026-01-03T01:00:00Z',
        committed_at: '2026-01-03T01:00:01Z',
        source_execution_id: E.e2,
        reverts_execution_id: E.e2,
      }),
      error: null,
    }),
    attempt({
      attempt_id: attemptId('440'),
      job_id: '00000213-0000-4000-8000-000000000213',
      execution_id: E.e4,
      migration_id: MIG_B,
      version: '4',
      action: 'forward',
      direction: 'up',
      status: 'succeeded',
      attempt_no: 1,
      started_at: '2026-01-04T00:00:00Z',
      finished_at: '2026-01-04T00:00:01Z',
      source_execution_id: E.e2,
      sql_entered: true,
      statements: [stmt(SQL_B_UP, 'succeeded')],
      commit: commitEntry({
        execution_id: E.e4,
        migration_id: MIG_B,
        version: '4',
        action: 'forward',
        direction: 'up',
        sql: SQL_B_UP,
        started_at: '2026-01-04T00:00:00Z',
        committed_at: '2026-01-04T00:00:01Z',
        source_execution_id: E.e2,
      }),
      error: null,
    }),
    attempt({
      attempt_id: attemptId('450'),
      job_id: '00000214-0000-4000-8000-000000000214',
      execution_id: E.e5,
      migration_id: MIG_C,
      version: '5',
      action: 'apply',
      direction: 'up',
      status: 'succeeded',
      attempt_no: 1,
      started_at: '2026-01-05T00:00:00Z',
      finished_at: '2026-01-05T00:00:01Z',
      source_execution_id: null,
      sql_entered: true,
      statements: [stmt(SQL_C_UP, 'succeeded')],
      commit: commitEntry({
        execution_id: E.e5,
        migration_id: MIG_C,
        version: '5',
        action: 'apply',
        direction: 'up',
        sql: SQL_C_UP,
        started_at: '2026-01-05T00:00:00Z',
        committed_at: '2026-01-05T00:00:01Z',
      }),
      error: null,
    }),
    attempt({
      attempt_id: attemptId('301'),
      job_id: '00000201-0000-4000-8000-000000000201',
      execution_id: '00000301-0000-4000-8000-000000000301',
      migration_id: MIG_A,
      version: null,
      action: 'apply',
      direction: 'up',
      status: 'running',
      attempt_no: 1,
      started_at: T0,
      finished_at: null,
      source_execution_id: null,
      sql_entered: true,
      statements: [stmt(SQL_A_UP, 'succeeded', 1, batchParams)],
      commit: null,
      error: null,
    }),
    attempt({
      attempt_id: attemptId('303'),
      job_id: '00000203-0000-4000-8000-000000000203',
      execution_id: '00000303-0000-4000-8000-000000000303',
      migration_id: MIG_A,
      version: null,
      action: 'apply',
      direction: 'up',
      status: 'failed',
      attempt_no: 1,
      started_at: T0,
      finished_at: T5,
      source_execution_id: null,
      sql_entered: false,
      statements: [],
      commit: null,
      error: REVIEW_STALE_ERROR,
    }),
    attempt({
      attempt_id: attemptId('304'),
      job_id: '00000204-0000-4000-8000-000000000204',
      execution_id: '00000304-0000-4000-8000-000000000304',
      migration_id: MIG_A,
      version: null,
      action: 'apply',
      direction: 'up',
      status: 'failed',
      attempt_no: 1,
      started_at: T0,
      finished_at: '2026-01-01T00:00:10Z',
      source_execution_id: null,
      sql_entered: true,
      statements: [stmt(SQL_A_UP, 'failed', 1, batchParams)],
      commit: null,
      error: SQL_FAILED_RETRYABLE,
    }),
    attempt({
      attempt_id: attemptId('305'),
      job_id: '00000205-0000-4000-8000-000000000205',
      execution_id: '00000305-0000-4000-8000-000000000305',
      migration_id: MIG_A,
      version: null,
      action: 'apply',
      direction: 'up',
      status: 'partial',
      attempt_no: 1,
      started_at: T0,
      finished_at: T5,
      source_execution_id: null,
      sql_entered: true,
      statements: [stmt(SQL_A_UP, 'succeeded', 1, batchParams)],
      commit: null,
      error: null,
    }),
    attempt({
      attempt_id: attemptId('306'),
      job_id: '00000206-0000-4000-8000-000000000206',
      execution_id: '00000306-0000-4000-8000-000000000306',
      migration_id: MIG_A,
      version: null,
      action: 'compensate',
      direction: 'down',
      status: 'partially_reverted',
      attempt_no: 1,
      started_at: T0,
      finished_at: T5,
      source_execution_id: '00000305-0000-4000-8000-000000000305',
      sql_entered: true,
      statements: [stmt(SQL_A_DOWN, 'succeeded', 1, { ids: IDS_1_42.slice(0, 10) })],
      commit: null,
      error: null,
    }),
    attempt({
      attempt_id: attemptId('307'),
      job_id: '00000207-0000-4000-8000-000000000207',
      execution_id: '00000307-0000-4000-8000-000000000307',
      migration_id: MIG_A,
      version: null,
      action: 'apply',
      direction: 'up',
      status: 'unknown',
      attempt_no: 1,
      started_at: T0,
      finished_at: null,
      source_execution_id: null,
      sql_entered: null,
      statements: [stmt(SQL_A_UP, 'unknown', 1, batchParams)],
      commit: null,
      error: null,
    }),
    attempt({
      attempt_id: attemptId('308'),
      job_id: '00000208-0000-4000-8000-000000000208',
      execution_id: '00000308-0000-4000-8000-000000000308',
      migration_id: MIG_A,
      version: null,
      action: 'apply',
      direction: 'up',
      status: 'partial',
      attempt_no: 1,
      started_at: T0,
      finished_at: T5,
      source_execution_id: null,
      sql_entered: true,
      statements: [stmt(SQL_A_UP, 'succeeded', 1, batchParams)],
      commit: null,
      error: null,
    }),
    attempt({
      attempt_id: attemptId('309'),
      job_id: '00000209-0000-4000-8000-000000000209',
      execution_id: '00000308-0000-4000-8000-000000000308',
      migration_id: MIG_A,
      version: '7',
      action: 'resume',
      direction: 'up',
      status: 'succeeded',
      attempt_no: 1,
      started_at: '2026-01-01T00:06:00Z',
      finished_at: '2026-01-01T00:07:00Z',
      source_execution_id: '00000308-0000-4000-8000-000000000308',
      sql_entered: true,
      statements: [],
      commit: commitEntry({
        execution_id: '00000308-0000-4000-8000-000000000308',
        migration_id: MIG_A,
        version: '7',
        action: 'resume',
        direction: 'up',
        sql: SQL_A_UP,
        started_at: '2026-01-01T00:06:00Z',
        committed_at: '2026-01-01T00:07:00Z',
        source_execution_id: '00000308-0000-4000-8000-000000000308',
      }),
      error: null,
    }),
    attempt({
      attempt_id: attemptId('099'),
      job_id: '50000000-0000-4000-8000-000000000099',
      execution_id: '60000000-0000-4000-8000-000000000099',
      migration_id: MIG_STD,
      version: '6',
      action: 'apply',
      direction: 'up',
      status: 'succeeded',
      attempt_no: 1,
      started_at: T0,
      finished_at: T5,
      source_execution_id: null,
      sql_entered: true,
      statements: [stmt(SQL_STD_UP, 'succeeded')],
      commit: commitEntry({
        execution_id: '60000000-0000-4000-8000-000000000099',
        migration_id: MIG_STD,
        version: '6',
        action: 'apply',
        direction: 'up',
        sql: SQL_STD_UP,
        started_at: T0,
        committed_at: T5,
      }),
      error: null,
    }),
    attempt({
      attempt_id: '80000000-0000-4000-8000-000000000002',
      job_id: null,
      execution_id: '60000000-0000-4000-8000-000000000098',
      migration_id: MIG_A,
      version: null,
      action: 'apply',
      direction: 'up',
      status: 'failed',
      attempt_no: 1,
      started_at: T5,
      finished_at: T5,
      source_execution_id: null,
      sql_entered: null,
      statements: [],
      commit: null,
      error: { code: 'LEGACY_FAILED', message: 'Eksekusi gagal', retryable: false },
      actor: null,
      legacy_execution_id: '60000000-0000-4000-8000-000000000098',
    }),
  ]
}
