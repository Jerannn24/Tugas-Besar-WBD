import { request } from './client'
import { pageParams, type PageQuery } from './paging'
import type { CursorPage, Execution, HistoryAttemptRow, HistoryEntry, ID } from './types'

const e = encodeURIComponent
const db = (databaseId: ID) => `/api/databases/${e(databaseId)}`
const devLimit = import.meta.env.DEV ? Number(import.meta.env.VITE_HISTORY_LIMIT) || undefined : undefined

export type HistoryQuery = PageQuery & { hideRollback?: boolean }

export function listHistory(databaseId: ID, q: HistoryQuery = {}) {
  const params = pageParams({ limit: devLimit, ...q })
  if (q.hideRollback) params.set('hide_rollback', 'true')
  return request<CursorPage<HistoryAttemptRow>>('GET', `${db(databaseId)}/migrations?${params}`)
}

export const isHistoryEntry = (x: Execution | HistoryEntry): x is HistoryEntry => 'batch_commits' in x
