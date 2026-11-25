import type {
  AccessLevel,
  Actor,
  AllowedAction,
  BatchConfig,
  Draft,
  DraftInputM2,
  ID,
  MigrationAction,
  MigrationMode,
  Proposal,
  Validation,
} from '../api/types'
import { fixtureGroupLeader } from './admin'
import { USERS } from './data'
import { failM2, okM2, parseBody } from './http'
import { acceptedView } from './jobs'
import { listResponse } from './paging'
import { REQUIRED_GROUPS, snapshotOf } from './samples'
import {
  editRequired,
  iso,
  isObject,
  isPosInt,
  isUuid,
  nextId,
  notFound,
  optText,
  type Ctx,
  type StoredJob,
} from './state'

const SQL_MAX_BYTES = 262_144
const ACTIONS: readonly MigrationAction[] = ['apply', 'rollback', 'forward', 'resume', 'compensate']
const MODES: readonly MigrationMode[] = ['standard', 'batch']

function draftView(d: Draft): Draft {
  return {
    id: d.id,
    database_id: d.database_id,
    title: d.title,
    sql: d.sql,
    revision: d.revision,
    created_by: d.created_by,
    updated_at: d.updated_at,
    applied_execution_id: d.applied_execution_id,
    sql_down: d.sql_down ?? null,
    irreversible_reason: d.irreversible_reason ?? null,
    mode: d.mode ?? 'standard',
    batch: d.batch ?? null,
  }
}

function currentLeader(groupId: ID, snapshot: ID | null): ID | null {
  const leader = fixtureGroupLeader(groupId)
  return leader === undefined ? snapshot : leader
}

function proposalView(p: Proposal, actor: Actor, access: AccessLevel): Proposal {
  const allowed: AllowedAction[] = []
  if (p.status === 'draft' && access === 'edit') allowed.push('submit_review')
  if (
    p.status === 'pending_review' &&
    p.approvals.some((a) => a.status === 'pending' && currentLeader(a.group_id, a.leader_user_id) === actor.id)
  ) {
    allowed.push('approve')
  }
  if (p.status === 'approved' && access === 'edit') allowed.push('enqueue')
  return {
    proposal_id: p.proposal_id,
    database_id: p.database_id,
    migration_id: p.migration_id,
    revision: p.revision,
    action: p.action,
    source_execution_id: p.source_execution_id,
    status: p.status,
    snapshot: p.snapshot,
    validation: p.validation,
    approvals: p.approvals,
    allowed_actions: allowed,
  }
}

function readDraftInput(body: unknown): { value: DraftInputM2 } | { errors: Record<string, string> } {
  const errors: Record<string, string> = {}
  const b = isObject(body) ? body : {}
  const title = typeof b.title === 'string' ? b.title.trim() : ''
  const sql = typeof b.sql === 'string' ? b.sql : ''
  if (!title) errors.title = 'Judul wajib diisi.'
  else if (title.length > 100) errors.title = 'Judul maksimal 100 karakter.'
  if (!sql.trim()) errors.sql = 'SQL wajib diisi.'
  else if (new TextEncoder().encode(sql).length > SQL_MAX_BYTES) errors.sql = 'SQL melebihi 262144 byte.'

  const sql_down = optText(b.sql_down)
  const irreversible_reason = optText(b.irreversible_reason)
  if (sql_down === undefined) errors.sql_down = 'SQL down harus teks atau null.'
  else if (sql_down !== null && new TextEncoder().encode(sql_down).length > SQL_MAX_BYTES) {
    errors.sql_down = 'SQL down melebihi 262144 byte.'
  }
  if (irreversible_reason === undefined) errors.irreversible_reason = 'Alasan harus teks atau null.'
  if (sql_down === null && irreversible_reason === null) {
    errors.sql_down = 'Isi SQL down atau alasan migration tidak dapat dibatalkan.'
  }

  const mode = b.mode === undefined ? 'standard' : b.mode
  if (!MODES.includes(mode as MigrationMode)) errors.mode = 'Mode harus standard atau batch.'

  let batch: BatchConfig | null = null
  if (mode === 'batch') {
    const c = isObject(b.batch) ? b.batch : null
    if (!c) errors.batch = 'Konfigurasi batch wajib untuk mode batch.'
    else {
      for (const key of ['schema', 'table', 'cursor_column'] as const) {
        if (typeof c[key] !== 'string' || !(c[key] as string).trim()) errors[`batch.${key}`] = 'Wajib diisi.'
      }
      if (!isPosInt(c.batch_size) || c.batch_size > 10_000) errors['batch.batch_size'] = 'Ukuran batch 1–10000.'
      if (!isObject(c.parameters)) errors['batch.parameters'] = 'Parameter harus objek.'
      batch = c as BatchConfig
    }
  } else if (b.batch !== undefined && b.batch !== null) {
    errors.batch = 'Mode standard tidak memakai konfigurasi batch.'
  }

  if (Object.keys(errors).length > 0) return { errors }
  return {
    value: {
      title,
      sql,
      sql_down: sql_down ?? null,
      irreversible_reason: irreversible_reason ?? null,
      mode: mode as MigrationMode,
      batch,
    },
  }
}

const FIXTURE_UNSUPPORTED = /^\s*(BEGIN|COMMIT|ROLLBACK|START\s+TRANSACTION|GRANT|REVOKE|CREATE\s+ROLE|ALTER\s+ROLE|SET\s+ROLE|DO)\b/im

function fixtureValidate(p: Proposal): Validation {
  const issues: Validation['issues'] = []
  if (FIXTURE_UNSUPPORTED.test(p.snapshot.sql_up)) {
    issues.push({ code: 'SQL_UNSUPPORTED', message: 'SQL up memuat kontrol transaksi atau perintah hak akses.' })
  }
  if (p.snapshot.sql_down && FIXTURE_UNSUPPORTED.test(p.snapshot.sql_down)) {
    issues.push({ code: 'SQL_UNSUPPORTED', message: 'SQL down memuat kontrol transaksi atau perintah hak akses.' })
  }
  return { status: issues.length > 0 ? 'failed' : 'passed', checked_at: iso(Date.now()), issues }
}

function findProposal(c: Ctx): Proposal | undefined {
  return c.state.proposals.find((p) => p.proposal_id === c.id && p.database_id === c.databaseId)
}

function readRevision(c: Ctx): number | Response {
  const b = parseBody<Record<string, unknown>>(c.req) ?? {}
  return isPosInt(b.revision)
    ? b.revision
    : failM2(400, 'INVALID_REQUEST', 'Permintaan tidak valid.', { revision: 'revision harus bilangan bulat ≥ 1.' })
}

function createProposal(c: Ctx): Response {
  if (c.access !== 'edit') return editRequired()
  const b = parseBody<Record<string, unknown>>(c.req) ?? {}
  const errors: Record<string, string> = {}
  if (!isUuid(b.migration_id)) errors.migration_id = 'migration_id harus UUID.'
  if (!isPosInt(b.revision)) errors.revision = 'revision harus bilangan bulat ≥ 1.'
  if (!ACTIONS.includes(b.action as MigrationAction)) errors.action = 'action tidak dikenal.'
  if (b.source_execution_id !== null && !isUuid(b.source_execution_id)) {
    errors.source_execution_id = 'source_execution_id harus UUID atau null.'
  }
  if (Object.keys(errors).length > 0) return failM2(400, 'INVALID_REQUEST', 'Permintaan tidak valid.', errors)

  const draft = c.state.drafts.find((d) => d.id === b.migration_id && d.database_id === c.databaseId)
  if (!draft) return notFound('Migration')
  if (draft.revision !== b.revision) {
    return failM2(409, 'VERSION_CONFLICT', 'Revisi draft sudah berubah. Muat ulang draft.')
  }
  const action = b.action as MigrationAction
  const source = (b.source_execution_id as ID | null) ?? null
  if (action === 'apply' && source !== null) {
    return failM2(422, 'VALIDATION_FAILED', 'Apply tidak memakai source execution.', {
      source_execution_id: 'Harus null untuk apply.',
    })
  }
  if (action !== 'apply' && !c.state.attempts.some((a) => a.execution_id === source && a.database_id === c.databaseId)) {
    return failM2(422, 'VALIDATION_FAILED', 'Source execution wajib untuk aksi ini.', {
      source_execution_id: 'Execution sumber tidak ditemukan.',
    })
  }

  const p: Proposal = {
    proposal_id: nextId(c.state, '00000700'),
    database_id: c.databaseId,
    migration_id: draft.id,
    revision: draft.revision,
    action,
    source_execution_id: source,
    status: 'draft',
    snapshot: snapshotOf(draft),
    validation: null,
    approvals: [],
    allowed_actions: [],
  }
  c.state.proposals.push(p)
  return okM2(proposalView(p, c.actor, c.access))
}

function submitReview(c: Ctx): Response {
  if (c.access !== 'edit') return editRequired()
  const p = findProposal(c)
  if (!p) return notFound('Proposal')
  const revision = readRevision(c)
  if (revision instanceof Response) return revision
  if (revision !== p.revision) return failM2(409, 'VERSION_CONFLICT', 'Revisi proposal sudah berubah. Muat ulang proposal.')
  if (p.status !== 'draft') return failM2(409, 'VERSION_CONFLICT', 'Proposal tidak lagi berstatus draft. Muat ulang proposal.')

  p.validation = fixtureValidate(p)
  if (p.validation.status === 'failed') {
    return failM2(422, 'VALIDATION_FAILED', 'Validasi gagal. Lihat hasil validasi pada proposal.')
  }
  p.status = 'pending_review'
  p.approvals = REQUIRED_GROUPS.map((g) => ({
    group_id: g.group_id,
    leader_user_id: currentLeader(g.group_id, g.leader_user_id),
    status: 'pending',
    approved_at: null,
  }))
  return okM2(proposalView(p, c.actor, c.access))
}

function approve(c: Ctx): Response {
  const p = findProposal(c)
  if (!p) return notFound('Proposal')
  const b = parseBody<Record<string, unknown>>(c.req) ?? {}
  const errors: Record<string, string> = {}
  if (!isPosInt(b.revision)) errors.revision = 'revision harus bilangan bulat ≥ 1.'
  if (!isUuid(b.group_id)) errors.group_id = 'group_id harus UUID.'
  if (Object.keys(errors).length > 0) return failM2(400, 'INVALID_REQUEST', 'Permintaan tidak valid.', errors)

  if (p.status === 'superseded') return failM2(409, 'REVIEW_STALE', 'Review proposal ini sudah usang.')
  if (b.revision !== p.revision) return failM2(409, 'VERSION_CONFLICT', 'Revisi proposal sudah berubah. Muat ulang proposal.')
  if (p.status !== 'pending_review') {
    return failM2(409, 'VERSION_CONFLICT', 'Proposal tidak sedang menunggu approval. Muat ulang proposal.')
  }
  const anchor = p.approvals.find((a) => a.group_id === b.group_id)
  if (!anchor) {
    return failM2(422, 'VALIDATION_FAILED', 'Group bukan approver wajib proposal ini.', { group_id: 'Group tidak wajib.' })
  }
  const swap = c.state.leader_swap.indexOf(p.proposal_id)
  if (swap >= 0) {
    c.state.leader_swap.splice(swap, 1)
    anchor.leader_user_id = c.actor.id === USERS.demo_editor.id ? USERS.operator.id : USERS.demo_editor.id
  }
  if (swap < 0) anchor.leader_user_id = currentLeader(anchor.group_id, anchor.leader_user_id)
  if (anchor.leader_user_id !== c.actor.id) {
    return failM2(403, 'LEADER_REQUIRED', 'Hanya leader group ini yang dapat menyetujui.')
  }

  const now = iso(Date.now())
  for (const a of p.approvals) {
    if (a.status === 'pending' && a !== anchor) a.leader_user_id = currentLeader(a.group_id, a.leader_user_id)
    if (a.leader_user_id === c.actor.id && a.status === 'pending') {
      a.status = 'approved'
      a.approved_at = now
    }
  }
  if (p.approvals.every((a) => a.status === 'approved')) p.status = 'approved'
  return okM2(proposalView(p, c.actor, c.access))
}

function enqueue(c: Ctx): Response {
  if (c.access !== 'edit') return editRequired()
  const p = findProposal(c)
  if (!p) return notFound('Proposal')
  const b = parseBody<Record<string, unknown>>(c.req) ?? {}
  const errors: Record<string, string> = {}
  if (!isUuid(b.request_id)) errors.request_id = 'request_id harus UUID.'
  if (!isPosInt(b.revision)) errors.revision = 'revision harus bilangan bulat ≥ 1.'
  if (Object.keys(errors).length > 0) return failM2(400, 'INVALID_REQUEST', 'Permintaan tidak valid.', errors)
  const requestId = b.request_id as string
  const revision = b.revision as number

  const prior = c.state.requests[requestId]
  if (prior) {
    const j = c.state.jobs.find((x) => x.job_id === prior.job_id)
    if (prior.proposal_id === p.proposal_id && prior.revision === revision && j) {
      return okM2({ ...acceptedView(j), replayed: true }, 200)
    }
    return failM2(409, 'REQUEST_CONFLICT', 'request_id sudah dipakai untuk permintaan lain.')
  }
  if (revision !== p.revision) return failM2(409, 'VERSION_CONFLICT', 'Revisi proposal sudah berubah. Muat ulang proposal.')
  if (p.status === 'superseded') return failM2(409, 'REVIEW_STALE', 'Review proposal ini sudah usang.')
  if (p.status === 'queued') {
    return failM2(409, 'REQUEST_CONFLICT', 'Proposal sudah diantrekan oleh request lain.')
  }
  if (p.status !== 'approved') return failM2(409, 'APPROVAL_REQUIRED', 'Proposal belum mendapat semua approval wajib.')

  const down = c.state.queue_down.indexOf(p.proposal_id)
  if (down >= 0) {
    c.state.queue_down.splice(down, 1)
    return failM2(503, 'QUEUE_UNAVAILABLE', 'Antrean sedang tidak tersedia. Job belum tersimpan.', {}, true)
  }

  const jobId = nextId(c.state, '00000900')
  const executionId = p.action === 'resume' && p.source_execution_id ? p.source_execution_id : nextId(c.state, '00000901')
  const job: StoredJob = {
    job_id: jobId,
    execution_id: executionId,
    proposal_id: p.proposal_id,
    database_id: c.databaseId,
    status: 'queued',
    status_url: `/api/databases/${c.databaseId}/jobs/${jobId}`,
    migration_id: p.migration_id,
    revision: p.revision,
    action: p.action,
    mode: p.snapshot.mode,
    execution_status: 'queued',
    attempt_no: 1,
    blocks_database: false,
    progress: null,
    source_execution_id: p.source_execution_id,
    created_at: iso(Date.now()),
    started_at: null,
    finished_at: null,
    next_retry_at: null,
    error: null,
    allowed_actions: [],
    sim_t0: Date.now(),
    sim_actor: c.actor,
  }
  c.state.jobs.push(job)
  c.state.requests[requestId] = { proposal_id: p.proposal_id, revision, job_id: jobId }
  p.status = 'queued'
  const drop = c.state.response_drop.indexOf(p.proposal_id)
  if (drop >= 0) {
    c.state.response_drop.splice(drop, 1)
    c.dropResponse = true
  }
  return okM2({ ...acceptedView(job), replayed: false }, 202)
}

function createDraft(c: Ctx): Response {
  if (c.access !== 'edit') return editRequired()
  const input = readDraftInput(parseBody<unknown>(c.req))
  if ('errors' in input) return failM2(422, 'VALIDATION_FAILED', 'Input tidak valid.', input.errors)
  const d: Draft = {
    id: nextId(c.state, '00000600'),
    database_id: c.databaseId,
    revision: 1,
    created_by: c.actor.id,
    updated_at: iso(Date.now()),
    applied_execution_id: null,
    ...input.value,
  }
  c.state.drafts.push(d)
  return okM2(draftView(d), 201)
}

function patchDraft(c: Ctx): Response {
  if (c.access !== 'edit') return editRequired()
  const d = c.state.drafts.find((x) => x.id === c.id && x.database_id === c.databaseId)
  if (!d) return notFound('Draft')
  const body = parseBody<Record<string, unknown>>(c.req) ?? {}
  if (!isPosInt(body.revision)) {
    return failM2(400, 'INVALID_REQUEST', 'Permintaan tidak valid.', { revision: 'revision harus bilangan bulat ≥ 1.' })
  }
  if (body.revision !== d.revision) {
    return failM2(409, 'VERSION_CONFLICT', 'Draft telah diubah di tempat lain. Muat ulang sebelum menyimpan.')
  }
  const input = readDraftInput(body)
  if ('errors' in input) return failM2(422, 'VALIDATION_FAILED', 'Input tidak valid.', input.errors)
  Object.assign(d, input.value, { revision: d.revision + 1, updated_at: iso(Date.now()) })
  for (const p of c.state.proposals) {
    if (p.migration_id === d.id && p.revision < d.revision && p.status !== 'queued') p.status = 'superseded'
  }
  return okM2(draftView(d))
}

export function workflowRoute(key: string, c: Ctx): Response | null {
  const { state, databaseId, req } = c
  switch (key) {
    case 'GET proposals':
      return listResponse(
        req,
        { db: databaseId, resource: 'proposals', hide: false },
        state.proposals
          .filter((p) => p.database_id === databaseId)
          .sort((a, b) => b.proposal_id.localeCompare(a.proposal_id))
          .map((p) => proposalView(p, c.actor, c.access)),
      )
    case 'POST proposals':
      return createProposal(c)
    case 'GET proposals/:id': {
      const p = findProposal(c)
      return p ? okM2(proposalView(p, c.actor, c.access)) : notFound('Proposal')
    }
    case 'POST proposals/:id/submit-review':
      return submitReview(c)
    case 'POST proposals/:id/approvals':
      return approve(c)
    case 'POST proposals/:id/jobs':
      return enqueue(c)

    case 'GET drafts':
      if (c.access !== 'edit') return editRequired()
      return okM2({
        items: state.drafts
          .filter((d) => d.database_id === databaseId)
          .sort((a, b) => b.updated_at.localeCompare(a.updated_at))
          .map(draftView),
      })
    case 'POST drafts':
      return createDraft(c)
    case 'GET drafts/:id': {
      if (c.access !== 'edit') return editRequired()
      const d = state.drafts.find((x) => x.id === c.id && x.database_id === databaseId)
      return d ? okM2(draftView(d)) : notFound('Draft')
    }
    case 'PATCH drafts/:id':
      return patchDraft(c)
    case 'POST drafts/:id/execute':
      return failM2(501, 'NOT_IMPLEMENTED', 'Eksekusi langsung belum tersedia. Gunakan proposal dan approval.')
  }
  return null
}
