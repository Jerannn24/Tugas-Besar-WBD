import type { TransportRequest } from '../api/client'
import type { Draft, Execution, ExecutionStatus, ID } from '../api/types'
import { DATABASES_BY_USER, TARGETS, USERS } from './data'
import { fail, ok, parseBody } from './http'

export const NETWORK_DROP = Symbol('network-drop')

type StoredDraft = Draft & { simulate_concurrent_edit?: boolean }
type StoredExecution = Execution & { resolve_at?: number }
type RequestRecord = { draft_id: ID; revision: number; execution_id: ID }
type State = {
  drafts: StoredDraft[]
  executions: StoredExecution[]
  requests: Record<string, RequestRecord>
  next_version: Record<ID, number>
  seq: number
}

const STORAGE_KEY = 'fixture_c06'
const RUNNING_MS = 6_000
const UNKNOWN_MS = 90_000
const SQL_MAX_BYTES = 256 * 1024
const UNSUPPORTED = /^\s*(BEGIN|COMMIT|ROLLBACK|START\s+TRANSACTION|GRANT|REVOKE|CREATE\s+ROLE|ALTER\s+ROLE|SET\s+ROLE|DO)\b/im

const MAIN = TARGETS.demo_main.id
const SECOND = TARGETS.demo_second.id
const editor = USERS.demo_editor
const admin = USERS.demo_admin

const draftId = (n: number) => `40000000-0000-4000-8000-${String(n).padStart(12, '0')}`
const executionId = (n: number) => `50000000-0000-4000-8000-${String(n).padStart(12, '0')}`

function seedDraft(n: number, title: string, sql: string, extra: Partial<StoredDraft> = {}): StoredDraft {
  return {
    id: draftId(n),
    database_id: MAIN,
    title,
    sql,
    revision: 1,
    created_by: editor.id,
    updated_at: `2026-10-${String(10 + n).padStart(2, '0')}T03:00:00Z`,
    applied_execution_id: null,
    ...extra,
  }
}

function seedExecution(
  n: number,
  database_id: ID,
  draft: ID,
  version: string,
  title: string,
  sql: string,
  status: ExecutionStatus,
  started_at: string,
  finished_at: string | null,
  error: Execution['error'] = null,
): StoredExecution {
  return {
    id: executionId(n),
    database_id,
    draft_id: draft,
    draft_revision: 1,
    version,
    title,
    sql,
    executed_by: { id: editor.id, username: editor.username },
    started_at,
    finished_at,
    status,
    error,
  }
}

function seed(): State {
  const createCustomers = 'CREATE TABLE public.customers (\n  id bigserial PRIMARY KEY,\n  email text NOT NULL UNIQUE\n);'
  const shipmentsSql = 'ALTER TABLE public.shipments ADD COLUMN carrier text;'
  const indexSql = 'CREATE INDEX orders_placed_at_idx ON public.orders (placed_at);'

  const drafts: StoredDraft[] = [
    seedDraft(1, 'Buat tabel customers', createCustomers, { applied_execution_id: executionId(1) }),
    seedDraft(2, 'Tambah kolom carrier ke shipments', `-- fixture:fail\n${shipmentsSql}`),
    seedDraft(3, 'Index orders.placed_at', indexSql, { applied_execution_id: executionId(3) }),
    seedDraft(4, 'Tambah kolom phone ke customers', 'ALTER TABLE public.customers ADD COLUMN phone text;'),
    seedDraft(
      5,
      'Draft konflik (stale revision)',
      "UPDATE public.customers SET status = 'closed' WHERE id = 3;",
      { simulate_concurrent_edit: true },
    ),
    seedDraft(6, 'Contoh running → succeeded', '-- fixture:running\nALTER TABLE public.events ADD COLUMN source text;'),
    seedDraft(7, 'Contoh unknown', '-- fixture:unknown\nDELETE FROM public.events WHERE kind = \'logout\';'),
    seedDraft(8, 'Contoh DATABASE_BUSY', '-- fixture:busy\nALTER TABLE public.orders ADD COLUMN note text;'),
    seedDraft(9, 'Contoh jaringan putus', '-- fixture:network\nCREATE INDEX events_kind_idx ON public.events (kind);'),
    seedDraft(10, 'Contoh SQL_UNSUPPORTED', 'BEGIN;\nALTER TABLE public.orders DROP COLUMN currency;\nCOMMIT;'),
  ]

  const executions: StoredExecution[] = [
    seedExecution(1, MAIN, draftId(1), '1', 'Buat tabel customers', createCustomers, 'succeeded', '2026-10-01T02:00:00Z', '2026-10-01T02:00:01Z'),
    seedExecution(2, MAIN, draftId(2), '2', 'Tambah kolom carrier ke shipments', `-- fixture:fail\n${shipmentsSql}`, 'failed', '2026-10-02T02:00:00Z', '2026-10-02T02:00:00Z', {
      code: '42P01',
      message: 'relation "public.shipments" does not exist',
    }),
    seedExecution(3, MAIN, draftId(3), '3', 'Index orders.placed_at', indexSql, 'succeeded', '2026-10-03T02:00:00Z', '2026-10-03T02:00:02Z'),
    seedExecution(11, SECOND, draftId(91), '1', 'Buat tabel products', 'CREATE TABLE inventory.products (sku text PRIMARY KEY);', 'succeeded', '2026-09-20T01:00:00Z', '2026-09-20T01:00:01Z'),
    {
      ...seedExecution(12, SECOND, draftId(92), '2', 'Hapus stok nol', 'DELETE FROM inventory.products WHERE stock = 0;', 'unknown', '2026-09-21T01:00:00Z', null),
      executed_by: { id: admin.id, username: admin.username },
    },
  ]

  return { drafts, executions, requests: {}, next_version: { [MAIN]: 4, [SECOND]: 3 }, seq: 100 }
}

let memory: State | null = null

function load(): State {
  try {
    const raw = sessionStorage.getItem(STORAGE_KEY)
    if (raw) return JSON.parse(raw) as State
  } catch {
    // storage tidak tersedia/rusak: pakai state di memory
  }
  return memory ?? seed()
}

function save(state: State): void {
  memory = state
  try {
    sessionStorage.setItem(STORAGE_KEY, JSON.stringify(state))
  } catch {
    // state hanya bertahan selama document ini
  }
}

const now = () => new Date().toISOString()
const unresolved = (x: Execution) => x.status === 'running' || x.status === 'unknown'

function settle(state: State): void {
  const t = Date.now()
  for (const x of state.executions) {
    if (unresolved(x) && x.resolve_at !== undefined && x.resolve_at <= t) {
      x.status = 'succeeded'
      x.finished_at = new Date(x.resolve_at).toISOString()
      delete x.resolve_at
      const draft = state.drafts.find((d) => d.id === x.draft_id)
      if (draft) draft.applied_execution_id = x.id
    }
  }
}

function draftView(d: StoredDraft): Draft {
  const { id, database_id, title, sql, revision, created_by, updated_at, applied_execution_id } = d
  return { id, database_id, title, sql, revision, created_by, updated_at, applied_execution_id }
}

function executionView(x: StoredExecution): Execution {
  const { resolve_at: _ignored, ...view } = x
  void _ignored
  return view
}

export function hasUnresolvedExecution(databaseId: ID): boolean {
  const state = load()
  settle(state)
  save(state)
  return state.executions.some((x) => x.database_id === databaseId && unresolved(x))
}

function executionResponse(x: StoredExecution): Response {
  return ok(executionView(x), unresolved(x) ? 202 : 200)
}

function validateDraftInput(input: { title?: unknown; sql?: unknown } | null): Record<string, string> {
  const errors: Record<string, string> = {}
  const title = typeof input?.title === 'string' ? input.title.trim() : ''
  const sql = typeof input?.sql === 'string' ? input.sql : ''
  if (title.length < 1 || title.length > 100) errors.title = 'Title harus 1 sampai 100 karakter.'
  if (sql.trim().length === 0) errors.sql = 'SQL wajib diisi.'
  else if (new TextEncoder().encode(sql).length > SQL_MAX_BYTES) errors.sql = 'SQL maksimal 256 KiB.'
  return errors
}

export function handleMigrationRequest(
  req: TransportRequest,
  username: string,
  databaseId: ID,
  rest: string[],
): Response | typeof NETWORK_DROP | null {
  const [resource, id, action] = rest
  if (resource !== 'drafts' && resource !== 'migrations') return null

  if (databaseId !== MAIN && databaseId !== SECOND) return fail(404, 'NOT_FOUND', 'Database tidak ditemukan.')
  const access = (DATABASES_BY_USER[username] ?? []).find((d) => d.id === databaseId)?.access
  if (!access) return fail(403, 'FORBIDDEN', 'Anda tidak memiliki akses ke database ini.')
  if (resource === 'drafts' && access !== 'edit') {
    return fail(403, 'FORBIDDEN', 'Draft hanya dapat diakses dengan akses edit.')
  }

  const state = load()
  settle(state)
  const key = `${req.method} ${resource}${id ? '/:id' : ''}${action ? `/${action}` : ''}`
  let result: Response | typeof NETWORK_DROP

  switch (key) {
    case 'GET migrations':
      result = ok({
        items: state.executions
          .filter((x) => x.database_id === databaseId)
          .sort((a, b) => Number(b.version) - Number(a.version))
          .map(executionView),
      })
      break
    case 'GET migrations/:id': {
      const x = state.executions.find((e) => e.id === id && e.database_id === databaseId)
      result = x ? ok(executionView(x)) : fail(404, 'NOT_FOUND', 'Execution tidak ditemukan.')
      break
    }
    case 'GET drafts':
      result = ok({
        items: state.drafts
          .filter((d) => d.database_id === databaseId)
          .sort((a, b) => b.updated_at.localeCompare(a.updated_at))
          .map(draftView),
      })
      break
    case 'POST drafts':
      result = createDraft(state, req, username, databaseId)
      break
    case 'GET drafts/:id': {
      const d = state.drafts.find((x) => x.id === id && x.database_id === databaseId)
      result = d ? ok(draftView(d)) : fail(404, 'NOT_FOUND', 'Draft tidak ditemukan.')
      break
    }
    case 'PATCH drafts/:id':
      result = patchDraft(state, req, databaseId, id)
      break
    case 'POST drafts/:id/execute':
      result = execute(state, req, username, databaseId, id)
      break
    default:
      return fail(404, 'NOT_FOUND', `Fixture belum tersedia untuk ${req.method} ${req.path}.`)
  }

  save(state)
  return result
}

function createDraft(state: State, req: TransportRequest, username: string, databaseId: ID): Response {
  const input = parseBody<{ title?: unknown; sql?: unknown }>(req)
  const errors = validateDraftInput(input)
  if (Object.keys(errors).length > 0) return fail(422, 'VALIDATION_FAILED', 'Input tidak valid.', errors)

  const draft: StoredDraft = {
    id: draftId(++state.seq),
    database_id: databaseId,
    title: (input!.title as string).trim(),
    sql: input!.sql as string,
    revision: 1,
    created_by: USERS[username].id,
    updated_at: now(),
    applied_execution_id: null,
  }
  state.drafts.push(draft)
  return ok(draftView(draft), 201)
}

function patchDraft(state: State, req: TransportRequest, databaseId: ID, id: ID): Response {
  const draft = state.drafts.find((d) => d.id === id && d.database_id === databaseId)
  if (!draft) return fail(404, 'NOT_FOUND', 'Draft tidak ditemukan.')

  const input = parseBody<{ title?: unknown; sql?: unknown; revision?: unknown }>(req)
  const errors = validateDraftInput(input)
  if (!Number.isInteger(input?.revision)) errors.revision = 'Revision wajib diisi.'
  if (Object.keys(errors).length > 0) return fail(422, 'VALIDATION_FAILED', 'Input tidak valid.', errors)

  if (draft.applied_execution_id) return fail(409, 'ALREADY_APPLIED', 'Draft sudah applied dan tidak dapat diubah.')
  if (state.executions.some((x) => x.draft_id === draft.id && unresolved(x))) {
    return fail(409, 'DATABASE_BUSY', 'Draft memiliki execution yang belum selesai.', {}, true)
  }

  if (draft.simulate_concurrent_edit) {
    delete draft.simulate_concurrent_edit
    draft.revision += 1
    draft.sql += '\n-- diubah oleh demo_admin di tab lain'
    draft.updated_at = now()
  }
  if (input!.revision !== draft.revision) {
    return fail(409, 'VERSION_CONFLICT', 'Draft telah diubah oleh pengguna lain.')
  }

  draft.title = (input!.title as string).trim()
  draft.sql = input!.sql as string
  draft.revision += 1
  draft.updated_at = now()
  return ok(draftView(draft))
}

function execute(
  state: State,
  req: TransportRequest,
  username: string,
  databaseId: ID,
  id: ID,
): Response | typeof NETWORK_DROP {
  const draft = state.drafts.find((d) => d.id === id && d.database_id === databaseId)
  if (!draft) return fail(404, 'NOT_FOUND', 'Draft tidak ditemukan.')

  const input = parseBody<{ revision?: unknown; request_id?: unknown }>(req)
  const revision = input?.revision
  const requestId = input?.request_id
  if (!Number.isInteger(revision) || typeof requestId !== 'string' || requestId === '') {
    return fail(422, 'VALIDATION_FAILED', 'Input tidak valid.', {
      ...(Number.isInteger(revision) ? {} : { revision: 'Revision wajib diisi.' }),
      ...(typeof requestId === 'string' && requestId !== '' ? {} : { request_id: 'request_id wajib diisi.' }),
    })
  }

  const dedupKey = `${databaseId}:${requestId}`
  const previous = state.requests[dedupKey]
  if (previous) {
    if (previous.draft_id !== draft.id || previous.revision !== revision) {
      return fail(409, 'REQUEST_CONFLICT', 'request_id sudah dipakai untuk permintaan lain.')
    }
    const x = state.executions.find((e) => e.id === previous.execution_id)!
    return executionResponse(x)
  }

  if (draft.applied_execution_id) return fail(409, 'ALREADY_APPLIED', 'Draft sudah applied.')
  if (revision !== draft.revision) {
    return fail(409, 'VERSION_CONFLICT', 'Revision yang dikirim bukan revision tersimpan terbaru.')
  }
  if (state.executions.some((x) => x.database_id === databaseId && unresolved(x)) || draft.sql.includes('-- fixture:busy')) {
    return fail(409, 'DATABASE_BUSY', 'Database target sedang menjalankan migration lain.', {}, true)
  }
  const unsupported = UNSUPPORTED.exec(draft.sql)
  if (unsupported) {
    return fail(
      422,
      'SQL_UNSUPPORTED',
      `Statement ${unsupported[1].toUpperCase()} tidak didukung: kontrol transaction/role diatur oleh platform.`,
    )
  }

  const started = Date.now()
  const user = USERS[username]
  const x: StoredExecution = {
    id: executionId(++state.seq),
    database_id: databaseId,
    draft_id: draft.id,
    draft_revision: draft.revision,
    version: String(state.next_version[databaseId]++),
    title: draft.title,
    sql: draft.sql,
    executed_by: { id: user.id, username: user.username },
    started_at: new Date(started).toISOString(),
    finished_at: new Date(started + 400).toISOString(),
    status: 'succeeded',
    error: null,
  }

  if (draft.sql.includes('-- fixture:fail')) {
    x.status = 'failed'
    x.error = { code: '42703', message: 'column "does_not_exist" of relation does not exist' }
  } else if (draft.sql.includes('-- fixture:running')) {
    x.status = 'running'
    x.finished_at = null
    x.resolve_at = started + RUNNING_MS
  } else if (draft.sql.includes('-- fixture:unknown')) {
    x.status = 'unknown'
    x.finished_at = null
    x.resolve_at = started + UNKNOWN_MS
  }

  if (x.status === 'succeeded') draft.applied_execution_id = x.id
  state.executions.push(x)
  state.requests[dedupKey] = { draft_id: draft.id, revision: draft.revision, execution_id: x.id }

  if (draft.sql.includes('-- fixture:network')) return NETWORK_DROP
  return executionResponse(x)
}
