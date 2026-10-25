import { useCallback, useEffect, useRef, useState } from 'react'
import type { ApiFailure } from '../../api/client'
import { getMigration } from '../../api/endpoints'
import type { DatabaseSummary, Execution, ID } from '../../api/types'
import { Button } from '../../components/Button'
import { FailureAlert } from '../../components/FailureAlert'
import { Loading } from '../../components/Loading'
import { Panel } from '../../components/Panel'
import { pageUrl } from '../../lib/query'
import { ExecutionDetail } from './ExecutionDetail'

type State =
  | { status: 'loading' }
  | { status: 'ready'; execution: Execution; refreshing: boolean }
  | { status: 'failed'; failure: ApiFailure }

export function ExecutionView({ database, executionId }: { database: DatabaseSummary; executionId: ID }) {
  const [state, setState] = useState<State>({ status: 'loading' })
  const seq = useRef(0)

  const load = useCallback(
    async (keepCurrent: boolean) => {
      const mine = ++seq.current
      setState((s) => (keepCurrent && s.status === 'ready' ? { ...s, refreshing: true } : { status: 'loading' }))
      const res = await getMigration(database.id, executionId)
      if (mine !== seq.current) return
      setState(
        res.ok ? { status: 'ready', execution: res.data, refreshing: false } : { status: 'failed', failure: res },
      )
    },
    [database.id, executionId],
  )

  useEffect(() => {
    void load(false)
  }, [load])

  if (state.status === 'loading') return <Loading label="Memuat detail execution…" />
  if (state.status === 'failed') {
    return (
      <FailureAlert failure={state.failure} notFoundTitle="Execution tidak ditemukan" onRetry={() => void load(false)} />
    )
  }

  const x = state.execution
  const unresolved = x.status === 'running' || x.status === 'unknown'
  return (
    <Panel
      title={`Execution v${x.version}`}
      actions={
        unresolved && (
          <Button variant="outline-secondary" pending={state.refreshing} onClick={() => void load(true)}>
            Cek status
          </Button>
        )
      }
    >
      <ExecutionDetail execution={x} />
      {database.access === 'edit' && (
        <p className="mt-3 mb-0">
          <a href={pageUrl('/migration/', { database: database.id, draft: x.draft_id })}>Buka draft asal</a>
        </p>
      )}
    </Panel>
  )
}
