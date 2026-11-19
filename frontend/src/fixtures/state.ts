import type { TransportRequest } from '../api/client'
import type { AccessLevel, Actor, Draft, ID, JobView, Proposal } from '../api/types'
import { failM2 } from './http'
import {
  QUEUE_DOWN_ONCE,
  seedAttempts,
  seedDrafts,
  seedJobs,
  seedProposals,
  type AttemptRecord,
} from './samples'

export type StoredJob = JobView & { sim_t0?: number; sim_actor?: Actor }
export type RequestRecord = { proposal_id: ID; revision: number; job_id: ID }
export type State = {
  drafts: Draft[]
  proposals: Proposal[]
  jobs: StoredJob[]
  attempts: AttemptRecord[]
  requests: Record<string, RequestRecord>
  queue_down: ID[]
  seq: number
  next_version: number
}

export type Ctx = {
  req: TransportRequest
  state: State
  actor: Actor
  access: AccessLevel
  databaseId: ID
  id: ID | undefined
}

export const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

const STORAGE_KEY = 'fixture_m2_v1'

let memory: State | null = null

function seed(): State {
  return {
    drafts: seedDrafts(),
    proposals: seedProposals(),
    jobs: seedJobs(),
    attempts: seedAttempts(),
    requests: {},
    queue_down: [...QUEUE_DOWN_ONCE],
    seq: 0,
    next_version: 8,
  }
}

export function load(): State {
  try {
    const raw = sessionStorage.getItem(STORAGE_KEY)
    if (raw) return JSON.parse(raw) as State
  } catch {
    // storage tidak tersedia/rusak: pakai state di memory
  }
  return memory ?? seed()
}

export function save(state: State): void {
  memory = state
  try {
    sessionStorage.setItem(STORAGE_KEY, JSON.stringify(state))
  } catch {
    // state hanya bertahan selama document ini
  }
}

export const iso = (ms: number) => new Date(ms).toISOString().replace(/\.\d{3}Z$/, 'Z')
export const nextId = (state: State, prefix: string) =>
  `${prefix}-0000-4000-8000-${String(++state.seq).padStart(12, '0')}`

export const isPosInt = (v: unknown): v is number => typeof v === 'number' && Number.isInteger(v) && v >= 1
export const isUuid = (v: unknown): v is string => typeof v === 'string' && UUID.test(v)
export const isObject = (v: unknown): v is Record<string, unknown> =>
  typeof v === 'object' && v !== null && !Array.isArray(v)
export const optText = (v: unknown): string | null | undefined =>
  v === undefined || v === null ? null : typeof v === 'string' ? (v.trim() === '' ? null : v) : undefined

export const notFound = (what: string) => failM2(404, 'NOT_FOUND', `${what} tidak ditemukan.`)
export const editRequired = () => failM2(403, 'FORBIDDEN', 'Aksi ini memerlukan akses edit.')
