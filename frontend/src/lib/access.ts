import { useCallback, useEffect, useRef, useState } from 'react'
import type { ApiFailure } from '../api/client'
import { listDatabases, listMigrations } from '../api/endpoints'
import type { DatabaseSummary, ID } from '../api/types'

export type DatabaseAccessState =
  | { status: 'loading' }
  | { status: 'ready'; database: DatabaseSummary }
  | { status: 'missing' }
  | { status: 'denied'; failure: ApiFailure }
  | { status: 'failed'; failure: ApiFailure }

async function resolveAccess(databaseId: ID, isCurrent: () => boolean): Promise<DatabaseAccessState | null> {
  const res = await listDatabases()
  if (!isCurrent()) return null
  if (!res.ok) return { status: 'failed', failure: res }

  const database = res.data.items.find((d) => d.id === databaseId)
  if (database) return { status: 'ready', database }

  const probe = await listMigrations(databaseId)
  if (!isCurrent()) return null
  
  if (!probe.ok && probe.kind === 'http' && [400, 403, 404].includes(probe.status)) {
    return { status: 'denied', failure: probe }
  }

  if (!probe.ok && probe.kind === 'http' && probe.status === 401) return { status: 'failed', failure: probe }
  return { status: 'missing' }
}

export function useDatabaseAccess(databaseId: ID) {
  const [state, setState] = useState<DatabaseAccessState>({ status: 'loading' })
  const seq = useRef(0)

  const fetchAccess = useCallback(async () => {
    const mine = ++seq.current
    const next = await resolveAccess(databaseId, () => mine === seq.current)
    if (next) setState(next)
  }, [databaseId])

  const reload = useCallback(async () => {
    setState({ status: 'loading' })
    await fetchAccess()
  }, [fetchAccess])

  useEffect(() => {
    void fetchAccess()
    const ref = seq
    return () => {
      ref.current++
    }
  }, [fetchAccess])

  return { state, reload }
}
