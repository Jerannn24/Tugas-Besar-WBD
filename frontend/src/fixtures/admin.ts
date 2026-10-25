import type { TransportRequest } from '../api/client'
import type { ConnectionInput, ConnectionSummary, Group, GroupGrants, ID, User } from '../api/types'
import { TARGETS, USERS } from './data'
import { fail, ok, parseBody } from './http'
import { hasUnresolvedExecution } from './migrations'

type Grant = GroupGrants['grants'][number]
type State = {
  users: User[]
  groups: Group[]
  members: Record<ID, ID[]>
  grants: Record<ID, Grant[]>
  connections: ConnectionSummary[]
  seq: number
}

const STORAGE_KEY = 'fixture_admin_v2'
const SSL_MODES: readonly string[] = ['disable', 'require', 'verify-full']

const READERS = '20000000-0000-4000-8000-000000000001'
const EDITORS = '20000000-0000-4000-8000-000000000002'
const STALE = '20000000-0000-4000-8000-000000000099'
const UNKNOWN_USER = '10000000-0000-4000-8000-000000000099'
const OFFLINE = '30000000-0000-4000-8000-000000000003'
const VIEW_ONLY = { executor_username: null, preparer_username: null, execution_ready: false }

function seed(): State {
  const u = USERS
  return {
    users: Object.values(u),
    groups: [
      { id: READERS, name: 'readers' },
      { id: EDITORS, name: 'editors' },
      { id: STALE, name: 'fixture_stale' },
    ],
    members: {
      [READERS]: [u.demo_viewer.id, u.demo_editor.id, u.demo_disabled.id],
      [EDITORS]: [u.demo_editor.id],
      [STALE]: [u.demo_viewer.id, UNKNOWN_USER],
    },
    grants: {
      [READERS]: [{ database_id: TARGETS.demo_main.id, access: 'view' }],
      [EDITORS]: [
        { database_id: TARGETS.demo_main.id, access: 'edit' },
        { database_id: TARGETS.demo_second.id, access: 'view' },
        { database_id: TARGETS.demo_readonly.id, access: 'edit' },
      ],
      [STALE]: [],
    },
    connections: [
      {
        ...TARGETS.demo_main,
        host: 'target-db',
        port: 5432,
        database_name: 'demo_main',
        username: 'demo_main_app',
        ssl_mode: 'disable',
        executor_username: 'demo_main_executor',
        preparer_username: 'demo_main_preparer',
        execution_ready: true,
      },
      { ...TARGETS.demo_second, host: 'target-db', port: 5432, database_name: 'demo_second', username: 'demo_second_app', ssl_mode: 'disable', ...VIEW_ONLY },
      { ...TARGETS.demo_readonly, host: 'target-db', port: 5432, database_name: 'demo_readonly', username: 'demo_readonly_app', ssl_mode: 'disable', ...VIEW_ONLY },
      { id: OFFLINE, name: 'demo_offline', host: 'offline.invalid', port: 5432, database_name: 'demo_offline', username: 'demo_offline_app', ssl_mode: 'require', ...VIEW_ONLY },
    ],
    seq: 100,
  }
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

function nextId(state: State, prefix: string): ID {
  state.seq += 1
  return `${prefix}-0000-4000-8000-${String(state.seq).padStart(12, '0')}`
}

const invalid = (field_errors: Record<string, string>, message = 'Input tidak valid.') =>
  fail(422, 'VALIDATION_FAILED', message, field_errors)
const notFound = (what: string) => fail(404, 'NOT_FOUND', `${what} tidak ditemukan.`)
const badRequest = () => fail(400, 'INVALID_REQUEST', 'Body JSON tidak valid.')
const isObject = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v)
const chars = (s: string) => [...s].length

export function handleAdminRequest(req: TransportRequest, username: string, rest: string[]): Response {
  if (!USERS[username]?.is_admin) return fail(403, 'FORBIDDEN', 'Hanya Platform Admin yang dapat mengakses fitur ini.')
  const state = load()
  const res = route(state, req, rest)
  if (req.method !== 'GET' && res.ok) save(state)
  return res
}

function route(state: State, req: TransportRequest, rest: string[]): Response {
  const [resource, id, sub] = rest
  const key = `${req.method} ${resource}${id ? '/:id' : ''}${sub ? `/${sub}` : ''}`
  const body = parseBody<unknown>(req)

  switch (key) {
    case 'GET users':
      return ok({ items: state.users })
    case 'POST users':
      return createUser(state, body)
    case 'PATCH users/:id':
      return patchUser(state, id, body)

    case 'GET groups':
      return ok({ items: state.groups })
    case 'POST groups':
      return createGroup(state, body)
    case 'PATCH groups/:id':
      return renameGroup(state, id, body)
    case 'DELETE groups/:id':
      return removeGroup(state, id)
    case 'GET groups/:id/members':
      return withGroup(state, id, () => ok({ group_id: id, user_ids: state.members[id] ?? [] }))
    case 'PUT groups/:id/members':
      return withGroup(state, id, () => putMembers(state, id, body))
    case 'GET groups/:id/grants':
      return withGroup(state, id, () => ok({ group_id: id, grants: state.grants[id] ?? [] }))
    case 'PUT groups/:id/grants':
      return withGroup(state, id, () => putGrants(state, id, body))

    case 'GET connections':
      return ok({ items: state.connections })
    case 'POST connections':
      return createConnection(state, body)
    case 'PATCH connections/:id':
      return patchConnection(state, id, body)
    case 'DELETE connections/:id':
      return removeConnection(state, id)
    case 'POST connections/:id/test':
      return testConnection(state, id)
  }
  return fail(404, 'NOT_FOUND', `Fixture belum tersedia untuk ${req.method} ${req.path}.`)
}

function validateUser(state: State, input: Record<string, unknown>, self: ID | null, creating: boolean) {
  const errors: Record<string, string> = {}
  if (creating || 'username' in input) {
    const name = typeof input.username === 'string' ? input.username.trim() : ''
    if (chars(name) < 3 || chars(name) > 64) errors.username = 'Username harus 3 sampai 64 karakter.'
    else if (state.users.some((u) => u.username === name && u.id !== self)) errors.username = 'Username sudah dipakai.'
  }
  if (creating || 'password' in input) {
    const pw = typeof input.password === 'string' ? input.password : ''
    if (chars(pw) < 12) errors.password = 'Password minimal 12 karakter.'
    else if (new TextEncoder().encode(pw).length > 1024) errors.password = 'Password maksimal 1 KiB.'
  }
  for (const flag of ['is_admin', 'is_active'] as const) {
    if ((creating && flag === 'is_admin') || flag in input) {
      if (typeof input[flag] !== 'boolean') errors[flag] = 'Nilai harus true atau false.'
    }
  }
  return errors
}

function createUser(state: State, body: unknown): Response {
  if (!isObject(body)) return badRequest()
  const errors = validateUser(state, body, null, true)
  if (Object.keys(errors).length > 0) return invalid(errors)
  const user: User = {
    id: nextId(state, '10000000'),
    username: (body.username as string).trim(),
    is_admin: body.is_admin as boolean,
    is_active: true,
  }
  state.users.push(user)
  return ok(user, 201)
}

function patchUser(state: State, id: ID, body: unknown): Response {
  const user = state.users.find((u) => u.id === id)
  if (!user) return notFound('User')
  if (!isObject(body)) return badRequest()
  const errors = validateUser(state, body, id, false)
  if (Object.keys(errors).length > 0) return invalid(errors)

  const next: User = {
    ...user,
    ...(typeof body.username === 'string' ? { username: body.username.trim() } : {}),
    ...(typeof body.is_admin === 'boolean' ? { is_admin: body.is_admin } : {}),
    ...(typeof body.is_active === 'boolean' ? { is_active: body.is_active } : {}),
  }
  const activeAdmins = state.users.map((u) => (u.id === id ? next : u)).filter((u) => u.is_admin && u.is_active)
  if (activeAdmins.length === 0) {
    const field = next.is_admin ? 'is_active' : 'is_admin'
    return invalid(
      { [field]: 'Harus tersisa minimal satu admin aktif.' },
      'Tidak dapat menonaktifkan atau menurunkan admin aktif terakhir.',
    )
  }
  Object.assign(user, next)
  return ok(user)
}

function withGroup(state: State, id: ID, fn: () => Response): Response {
  return state.groups.some((g) => g.id === id) ? fn() : notFound('Group')
}

function groupName(state: State, body: unknown, self: ID | null): string | Response {
  if (!isObject(body)) return badRequest()
  const name = typeof body.name === 'string' ? body.name.trim() : ''
  if (chars(name) < 1 || chars(name) > 100) return invalid({ name: 'Nama group harus 1 sampai 100 karakter.' })
  if (state.groups.some((g) => g.name === name && g.id !== self)) return invalid({ name: 'Nama group sudah dipakai.' })
  return name
}

function createGroup(state: State, body: unknown): Response {
  const name = groupName(state, body, null)
  if (typeof name !== 'string') return name
  const group: Group = { id: nextId(state, '20000000'), name }
  state.groups.push(group)
  state.members[group.id] = []
  state.grants[group.id] = []
  return ok(group, 201)
}

function renameGroup(state: State, id: ID, body: unknown): Response {
  const group = state.groups.find((g) => g.id === id)
  if (!group) return notFound('Group')
  const name = groupName(state, body, id)
  if (typeof name !== 'string') return name
  group.name = name
  return ok(group)
}

function removeGroup(state: State, id: ID): Response {
  if (!state.groups.some((g) => g.id === id)) return notFound('Group')
  state.groups = state.groups.filter((g) => g.id !== id)
  delete state.members[id]
  delete state.grants[id]
  return ok({ deleted: true })
}

function putMembers(state: State, id: ID, body: unknown): Response {
  if (!isObject(body) || !Array.isArray(body.user_ids) || !body.user_ids.every((x) => typeof x === 'string')) {
    return badRequest()
  }
  const ids = body.user_ids as string[]
  if (new Set(ids).size !== ids.length) return invalid({ user_ids: 'Ada ID user yang duplikat.' })
  const unknown = ids.filter((x) => !state.users.some((u) => u.id === x))
  if (unknown.length > 0) {
    return invalid({ user_ids: `ID user tidak dikenal: ${unknown.join(', ')}` }, 'Anggota tidak disimpan karena ada ID yang tidak dikenal.')
  }
  state.members[id] = [...ids].sort()
  return ok({ group_id: id, user_ids: state.members[id] })
}

function putGrants(state: State, id: ID, body: unknown): Response {
  if (!isObject(body) || !Array.isArray(body.grants)) return badRequest()
  const grants: Grant[] = []
  for (const g of body.grants) {
    if (!isObject(g) || typeof g.database_id !== 'string') return badRequest()
    if (g.access !== 'view' && g.access !== 'edit') return invalid({ grants: 'Access harus view atau edit.' })
    grants.push({ database_id: g.database_id, access: g.access })
  }
  if (new Set(grants.map((g) => g.database_id)).size !== grants.length) {
    return invalid({ grants: 'Satu database hanya boleh punya satu grant per group.' })
  }
  const unknown = grants.filter((g) => !state.connections.some((c) => c.id === g.database_id))
  if (unknown.length > 0) {
    return invalid(
      { grants: `Database tidak dikenal: ${unknown.map((g) => g.database_id).join(', ')}` },
      'Grant tidak disimpan karena ada database yang tidak dikenal.',
    )
  }
  state.grants[id] = grants.sort((a, b) => a.database_id.localeCompare(b.database_id))
  return ok({ group_id: id, grants: state.grants[id] })
}

const TEXT_FIELDS = ['name', 'host', 'database_name', 'username'] as const
const ROLES = ['executor', 'preparer'] as const

function validateCredentials(input: Record<string, unknown>, self: ConnectionSummary | null, errors: Record<string, string>) {
  const sent = ROLES.filter((r) => r in input)
  const configured = (r: (typeof ROLES)[number]) => Boolean(self?.[`${r}_username`])
  const missing = ROLES.find((r) => !sent.includes(r))
  if (sent.length === 1 && missing && !configured(missing)) errors[missing] = 'Executor dan preparer harus dikirim bersama.'
  for (const r of sent) {
    const v = input[r]
    if (!isObject(v)) {
      errors[r] = 'Harus berupa objek {username, password}.'
      continue
    }
    if (typeof v.username !== 'string' || v.username.trim() === '') errors[`${r}.username`] = 'Username tidak boleh kosong.'
    const needsPassword = self === null || !configured(r)
    if ('password' in v || needsPassword) {
      if (typeof v.password !== 'string' || v.password === '') {
        errors[`${r}.password`] = needsPassword ? 'Password wajib diisi.' : 'Password tidak boleh kosong.'
      }
    }
  }
}
const sendsCredentials = (input: Record<string, unknown>) => ROLES.some((r) => r in input)
const unreachable = (host: string) => host.toLowerCase().endsWith('.invalid')
const unverified = () => fail(502, 'TARGET_UNAVAILABLE', 'Kredensial tidak dapat diverifikasi ke database target.', {}, true)

const targetKey = (c: Pick<ConnectionInput, 'host' | 'port' | 'database_name'>) =>
  JSON.stringify([c.host.trim().toLowerCase(), c.port, c.database_name.trim()])

function validateConnection(state: State, input: Record<string, unknown>, self: ConnectionSummary | null) {
  const creating = self === null
  const errors: Record<string, string> = {}
  for (const f of TEXT_FIELDS) {
    if (!creating && !(f in input)) continue
    const v = typeof input[f] === 'string' ? (input[f] as string).trim() : ''
    if (v === '') errors[f] = 'Wajib diisi.'
    else if (f === 'name' && chars(v) > 100) errors.name = 'Nama maksimal 100 karakter.'
  }
  if (creating || 'port' in input) {
    const p = input.port
    if (typeof p !== 'number' || !Number.isInteger(p) || p < 1 || p > 65535) errors.port = 'Port harus 1 sampai 65535.'
  }
  if (creating || 'password' in input) {
    if (typeof input.password !== 'string' || input.password === '') errors.password = 'Password tidak boleh kosong.'
  }
  if (creating || 'ssl_mode' in input) {
    if (typeof input.ssl_mode !== 'string' || !SSL_MODES.includes(input.ssl_mode)) {
      errors.ssl_mode = 'SSL mode harus disable, require, atau verify-full.'
    }
  }
  validateCredentials(input, self, errors)
  if (Object.keys(errors).length === 0) {
    const merged = { ...self, ...input } as ConnectionInput
    const key = targetKey(merged)
    if (state.connections.some((c) => c.id !== self?.id && targetKey(c) === key)) {
      errors.host = 'Target dengan host, port, dan nama database yang sama sudah terdaftar.'
    }
  }
  return errors
}

function summary(input: Record<string, unknown>, base: ConnectionSummary): ConnectionSummary {
  const s = (f: (typeof TEXT_FIELDS)[number]) => (typeof input[f] === 'string' ? (input[f] as string).trim() : base[f])
  const role = (r: (typeof ROLES)[number]) => {
    const v = input[r]
    return isObject(v) && typeof v.username === 'string' ? v.username.trim() : (base[`${r}_username`] ?? null)
  }
  const executor_username = role('executor')
  const preparer_username = role('preparer')
  return {
    id: base.id,
    name: s('name'),
    host: s('host'),
    port: typeof input.port === 'number' ? input.port : base.port,
    database_name: s('database_name'),
    username: s('username'),
    ssl_mode: (typeof input.ssl_mode === 'string' ? input.ssl_mode : base.ssl_mode) as ConnectionSummary['ssl_mode'],
    executor_username,
    preparer_username,
    execution_ready: Boolean(executor_username && preparer_username),
  }
}

function createConnection(state: State, body: unknown): Response {
  if (!isObject(body)) return badRequest()
  const errors = validateConnection(state, body, null)
  if (Object.keys(errors).length > 0) return invalid(errors)
  if (sendsCredentials(body) && unreachable(String(body.host))) return unverified()
  const empty: ConnectionSummary = { id: nextId(state, '30000000'), name: '', host: '', port: 0, database_name: '', username: '', ssl_mode: 'require', ...VIEW_ONLY }
  const created = summary(body, empty) // password tidak disimpan di fixture
  state.connections.push(created)
  return ok(created, 201)
}

function patchConnection(state: State, id: ID, body: unknown): Response {
  const current = state.connections.find((c) => c.id === id)
  if (!current) return notFound('Connection')
  if (!isObject(body)) return badRequest()
  const errors = validateConnection(state, body, current)
  if (Object.keys(errors).length > 0) return invalid(errors)
  const next = summary(body, current)
  if (sendsCredentials(body) && unreachable(next.host)) return unverified()
  const identityChanged = targetKey(next) !== targetKey(current)
  if (identityChanged && hasUnresolvedExecution(id)) {
    return fail(409, 'DATABASE_BUSY', 'Target sedang menjalankan migration; host, port, dan database tidak dapat diubah.', {}, true)
  }
  Object.assign(current, next)
  return ok(current)
}

function removeConnection(state: State, id: ID): Response {
  if (!state.connections.some((c) => c.id === id)) return notFound('Connection')
  if (hasUnresolvedExecution(id)) {
    return fail(409, 'DATABASE_BUSY', 'Target sedang menjalankan migration; connection tidak dapat dihapus.', {}, true)
  }
  state.connections = state.connections.filter((c) => c.id !== id)
  for (const groupId of Object.keys(state.grants)) {
    state.grants[groupId] = state.grants[groupId].filter((g) => g.database_id !== id)
  }
  return ok({ deleted: true })
}

function testConnection(state: State, id: ID): Response {
  const c = state.connections.find((x) => x.id === id)
  if (!c) return notFound('Connection')
  if (unreachable(c.host)) {
    return fail(502, 'TARGET_UNAVAILABLE', 'Database target tidak dapat dihubungi.', {}, true)
  }
  return ok({ reachable: true })
}
