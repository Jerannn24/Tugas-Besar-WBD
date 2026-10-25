import type { LoginInput, SessionView } from '../api/types'
import type { TransportRequest } from '../api/client'
import { DATABASES_BY_USER, FIXTURE_CSRF_TOKEN, SESSION_LIFETIME_MS, USERS } from './data'
import { VIEWER, tableKey } from './viewer'
import { fail, ok, parseBody } from './http'
import { NETWORK_DROP, handleMigrationRequest } from './migrations'
import { handleAdminRequest } from './admin'

const SESSION_KEY = 'fixture_user'

type Handler = (req: TransportRequest) => Response

const routes: Record<string, Handler> = {
  'GET /api/csrf': () => ok({ csrf_token: FIXTURE_CSRF_TOKEN }),
  'POST /api/login': login,
  'GET /api/session': (req) => withUser(req, (username) => ok(sessionView(username))),
  'POST /api/logout': (req) =>
    withUser(req, () => {
      writeSessionUser(null)
      return ok({ logged_out: true })
    }),
  'GET /api/databases': (req) =>
    withUser(req, (username) => ok({ items: DATABASES_BY_USER[username] ?? [] })),
}

export async function handleFixtureRequest(req: TransportRequest): Promise<Response> {
  await new Promise((r) => setTimeout(r, 250)) 
  const path = req.path.split('?')[0]
  const viewer = VIEWER_ROUTE.exec(path)
  if (req.method === 'GET' && viewer) return viewerRequest(req, decodeURIComponent(viewer[1]), viewer[2])
  const database = DATABASE_ROUTE.exec(path)
  if (database) return databaseRequest(req, decodeURIComponent(database[1]), database[2])
  if (path.startsWith(ADMIN_PREFIX)) {
    const rest = path.slice(ADMIN_PREFIX.length).split('/').map(decodeURIComponent)
    return withUser(req, (username) => handleAdminRequest(req, username, rest))
  }
  const handler = routes[`${req.method} ${path}`]
  if (!handler) return fail(404, 'NOT_FOUND', `Fixture belum tersedia untuk ${req.method} ${path}.`)
  return handler(req)
}

const ADMIN_PREFIX = '/api/admin/'

const DATABASE_ROUTE = /^\/api\/databases\/([^/]+)\/(.+)$/

function databaseRequest(req: TransportRequest, databaseId: string, rest: string): Response {
  let dropped = false
  const res = withUser(req, (username) => {
    const result = handleMigrationRequest(req, username, databaseId, rest.split('/').map(decodeURIComponent))
    if (result === NETWORK_DROP) {
      dropped = true
      return ok(null)
    }
    return result ?? fail(404, 'NOT_FOUND', `Fixture belum tersedia untuk ${req.method} ${req.path}.`)
  })
  if (dropped) throw new TypeError('Failed to fetch (simulasi fixture)')
  return res
}

const VIEWER_ROUTE = /^\/api\/databases\/([^/]+)\/(schemas|table|rows)$/
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
const schemasFailedOnce = new Set<string>()

async function viewerRequest(req: TransportRequest, databaseId: string, resource: string): Promise<Response> {
  await new Promise((r) => setTimeout(r, 150 + Math.random() * 750))
  return withUser(req, (username) => {
    if (!UUID.test(databaseId)) return fail(400, 'INVALID_REQUEST', 'Invalid database id')
    const db = VIEWER[databaseId]
    if (!db) return fail(404, 'NOT_FOUND', 'Database not found')
    if (!(DATABASES_BY_USER[username] ?? []).some((d) => d.id === databaseId)) {
      return fail(403, 'FORBIDDEN', 'Anda tidak memiliki akses ke database ini.')
    }

    if (resource === 'schemas') {
      if (db.failFirstSchemas && !schemasFailedOnce.has(databaseId)) {
        schemasFailedOnce.add(databaseId)
        return fail(502, 'TARGET_UNAVAILABLE', 'Database target tidak dapat dihubungi.', {}, true)
      }
      return ok({ items: db.schemas })
    }

    const query = new URLSearchParams(req.path.split('?')[1] ?? '')
    const schema = query.get('schema') ?? ''
    const tableName = query.get('table') ?? ''
    if (!schema || !tableName) {
      return fail(400, 'INVALID_REQUEST', 'Parameter schema dan table wajib diisi.')
    }
    const key = tableKey(schema, tableName)
    if (db.unavailableTables.has(key)) {
      return fail(502, 'TARGET_UNAVAILABLE', 'Database target tidak dapat dihubungi.', {}, true)
    }
    const found = db.tables.get(key)
    if (!found) return fail(404, 'NOT_FOUND', 'Table tidak ditemukan.')

    if (resource === 'table') return ok(found.meta)

    const limit = Number(query.get('limit') ?? '50')
    const offset = Number(query.get('offset') ?? '0')
    if (!Number.isInteger(limit) || limit < 1 || limit > 200 || !Number.isInteger(offset) || offset < 0) {
      return fail(400, 'INVALID_REQUEST', 'Parameter limit atau offset tidak valid.')
    }
    return ok({
      columns: found.meta.columns.map((c) => ({ name: c.name, data_type: c.data_type })),
      rows: found.rows.slice(offset, offset + limit),
      limit,
      offset,
      has_more: offset + limit < found.rows.length,
    })
  })
}

function login(req: TransportRequest): Response {
  if (!csrfValid(req)) return fail(403, 'CSRF_INVALID', 'Token CSRF tidak valid.')
  const input = parseBody<Partial<LoginInput>>(req)
  const username = typeof input?.username === 'string' ? input.username.trim() : ''
  const password = typeof input?.password === 'string' ? input.password : ''

  const fieldErrors: Record<string, string> = {}
  if (!username) fieldErrors.username = 'Username wajib diisi.'
  if (!password) fieldErrors.password = 'Password wajib diisi.'
  if (Object.keys(fieldErrors).length > 0) {
    return fail(422, 'VALIDATION_FAILED', 'Input tidak valid.', fieldErrors)
  }

  const user = USERS[username]
  if (!user || !user.is_active) {
    return fail(401, 'INVALID_CREDENTIALS', 'Username atau password salah.')
  }
  writeSessionUser(username)
  return ok(sessionView(username))
}

function withUser(req: TransportRequest, fn: (username: string) => Response): Response {
  const username = readSessionUser()
  if (!username || !USERS[username]?.is_active) {
    return fail(401, 'UNAUTHENTICATED', 'Sesi tidak valid atau telah berakhir.')
  }
  if (req.method !== 'GET' && !csrfValid(req)) {
    return fail(403, 'CSRF_INVALID', 'Token CSRF tidak valid.')
  }
  return fn(username)
}

function sessionView(username: string): SessionView {
  return {
    user: USERS[username],
    absolute_expires_at: new Date(Date.now() + SESSION_LIFETIME_MS).toISOString(),
  }
}

function csrfValid(req: TransportRequest): boolean {
  return req.headers['X-CSRF-Token'] === FIXTURE_CSRF_TOKEN
}

function readSessionUser(): string | null {
  try {
    return sessionStorage.getItem(SESSION_KEY)
  } catch {
    return null
  }
}

function writeSessionUser(username: string | null): void {
  try {
    if (username) sessionStorage.setItem(SESSION_KEY, username)
    else sessionStorage.removeItem(SESSION_KEY)
  } catch {
    // storage tidak tersedia
  }
}
