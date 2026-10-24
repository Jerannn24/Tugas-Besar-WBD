import { useCallback, useEffect, useState } from 'react'
import type { ApiFailure } from '../api/client'
import { listDatabases } from '../api/endpoints'
import type { DatabaseSummary, ID } from '../api/types'

export type DatabaseAccessState =
  | { status: 'loading' }
  | { status: 'ready'; database: DatabaseSummary }
  | { status: 'missing' }
  | { status: 'failed'; failure: ApiFailure }

export function useDatabaseAccess(databaseId: ID) {
  const [state, setState] = useState<DatabaseAccessState>({ status: 'loading' })

  const reload = useCallback(async () => {
    setState({ status: 'loading' })
    const res = await listDatabases()
    if (!res.ok) {
      setState({ status: 'failed', failure: res })
      return
    }
    const database = res.data.items.find((d) => d.id === databaseId)
    setState(database ? { status: 'ready', database } : { status: 'missing' })
  }, [databaseId])

  useEffect(() => {
    void reload()
  }, [reload])

  return { state, reload }
}
