import type { TransportRequest } from '../api/client'
import type { CursorPage, ID } from '../api/types'
import { failM2, okM2 } from './http'

export type Scope = { db: ID; resource: string; hide: boolean }
type PageWindow = { limit: number; offset: number }
type CursorBody = { d: string; r: string; h: boolean; o: number }

export const query = (req: TransportRequest) => new URLSearchParams(req.path.split('?')[1] ?? '')

function readWindow(req: TransportRequest, scope: Scope): PageWindow | Response {
  const q = query(req)
  let limit = 20
  const rawLimit = q.get('limit')
  if (rawLimit !== null) {
    limit = /^\d+$/.test(rawLimit) ? Number(rawLimit) : NaN
    if (!(limit >= 1 && limit <= 100)) {
      return failM2(400, 'INVALID_REQUEST', 'Parameter limit tidak valid.', { limit: 'Limit harus 1–100.' })
    }
  }
  const rawCursor = q.get('cursor')
  if (rawCursor === null) return { limit, offset: 0 }

  const invalid = failM2(400, 'INVALID_REQUEST', 'Cursor tidak valid untuk daftar ini.', { cursor: 'Muat ulang dari halaman pertama.' })
  let body: Partial<CursorBody>
  try {
    body = JSON.parse(atob(rawCursor)) as Partial<CursorBody>
  } catch {
    return invalid
  }
  if (
    body.d !== scope.db ||
    body.r !== scope.resource ||
    body.h !== scope.hide ||
    typeof body.o !== 'number' ||
    !Number.isInteger(body.o) ||
    body.o < 0
  ) {
    return invalid
  }
  return { limit, offset: body.o }
}

function pageOf<T>(items: T[], w: PageWindow, scope: Scope): CursorPage<T> {
  const end = w.offset + w.limit
  const cursor: CursorBody = { d: scope.db, r: scope.resource, h: scope.hide, o: end }
  return { items: items.slice(w.offset, end), next_cursor: end < items.length ? btoa(JSON.stringify(cursor)) : null }
}

export function listResponse<T>(req: TransportRequest, scope: Scope, items: T[], extra: Record<string, unknown> = {}): Response {
  const w = readWindow(req, scope)
  if (w instanceof Response) return w
  return okM2({ ...pageOf(items, w, scope), ...extra })
}
