import { request } from './client'
import { pageParams, type PageQuery } from './paging'
import type { CursorPage, HistoryAttemptRow, ID } from './types'

const e = encodeURIComponent
const db = (databaseId: ID) => `/api/databases/${e(databaseId)}`

export type HistoryQuery = PageQuery & { hideRollback?: boolean }

export function listHistory(databaseId: ID, q: HistoryQuery = {}) {
  const params = pageParams(q)
  if (q.hideRollback) params.set('hide_rollback', 'true')
  return request<CursorPage<HistoryAttemptRow>>('GET', `${db(databaseId)}/migrations?${params}`)
}
