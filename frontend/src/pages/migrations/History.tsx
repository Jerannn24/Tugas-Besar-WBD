import { useCallback } from 'react'
import { useSearchParams } from 'react-router'
import { listHistory } from '../../api/history'
import type { HistoryAttemptRow, ID } from '../../api/types'
import { Button } from '../../components/Button'
import { CheckField } from '../../components/CheckField'
import { EmptyState } from '../../components/EmptyState'
import { FailureAlert } from '../../components/FailureAlert'
import { Panel } from '../../components/Panel'
import { WorkflowAlert } from '../../components/WorkflowAlert'
import { useCursorList } from '../../lib/cursor'
import { Attempt } from './Attempt'

const keyOf = (r: HistoryAttemptRow) => r.attempt_id

function HistorySkeleton() {
  return (
    <div className="placeholder-glow" role="status" aria-busy="true" aria-live="polite">
      <span className="visually-hidden">Memuat riwayat migration…</span>
      <ol className="history-list" aria-hidden="true">
        {Array.from({ length: 3 }, (_, i) => (
          <li key={i} className="history-item card">
            <div className="card-body">
              <div className="d-flex gap-2 mb-3">
                <span className="placeholder col-2" />
                <span className="placeholder col-2" />
                <span className="placeholder col-1" />
              </div>
              <div className="history-meta">
                {Array.from({ length: 4 }, (_, j) => (
                  <span key={j} className="placeholder col-8" />
                ))}
              </div>
            </div>
          </li>
        ))}
      </ol>
    </div>
  )
}

function AttemptList({ databaseId, hide }: { databaseId: ID; hide: boolean }) {
  const loader = useCallback(
    (cursor: string | null) => listHistory(databaseId, { cursor, hideRollback: hide }),
    [databaseId, hide],
  )
  const { state, reload, loadMore } = useCursorList(loader, keyOf)

  return (
    <>
      <div className="d-flex justify-content-end mb-2">
        <Button variant="outline-secondary" disabled={state.status === 'loading'} onClick={() => void reload()}>
          Muat ulang
        </Button>
      </div>
      {state.status === 'loading' && <HistorySkeleton />}
      {state.status === 'failed' && <FailureAlert failure={state.failure} onRetry={() => void reload()} />}
      {state.status === 'ready' &&
        (state.items.length === 0 ? (
          <EmptyState title="Belum ada riwayat">
            {hide
              ? 'Tidak ada attempt yang tersisa setelah pasangan rollback disembunyikan.'
              : 'Belum ada migration yang dijalankan pada database ini.'}
          </EmptyState>
        ) : (
          <>
            <ol className="history-list" aria-label="Riwayat migration per attempt">
              {state.items.map((row) => (
                <Attempt key={row.attempt_id} databaseId={databaseId} row={row} />
              ))}
            </ol>
            <WorkflowAlert failure={state.moreFailure} title="Gagal memuat halaman berikutnya" />
            {state.next !== null && (
              <div className="mt-3">
                <Button variant="outline-secondary" pending={state.more} onClick={() => void loadMore()}>
                  Muat lebih banyak
                </Button>
              </div>
            )}
          </>
        ))}
    </>
  )
}

export function History({ databaseId }: { databaseId: ID }) {
  const [search, setSearch] = useSearchParams()
  const hide = search.get('hide_rollback') === 'true'

  const toggle = (on: boolean) =>
    setSearch(
      (prev) => {
        const next = new URLSearchParams(prev)
        if (on) next.set('hide_rollback', 'true')
        else next.delete('hide_rollback')
        return next
      },
      { replace: true },
    )

  return (
    <Panel title="Riwayat migration">
      <CheckField
        label="Hide rollback"
        checked={hide}
        onChange={toggle}
        hint="Sembunyikan pasangan migration dan rollback sukses yang saling membatalkan. Attempt gagal tetap tampil."
      />
      <AttemptList key={String(hide)} databaseId={databaseId} hide={hide} />
    </Panel>
  )
}
