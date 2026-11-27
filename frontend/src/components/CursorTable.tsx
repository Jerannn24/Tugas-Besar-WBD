import type { ReactNode } from 'react'
import type { CursorState } from '../lib/cursor'
import { Button } from './Button'
import { DataTable, type DataColumn } from './DataTable'
import { EmptyState } from './EmptyState'
import { FailureAlert } from './FailureAlert'
import { TableSkeleton } from './Skeleton'
import { WorkflowAlert } from './WorkflowAlert'

type Props<T> = {
  state: CursorState<T>
  columns: DataColumn<T>[]
  rowKey: (row: T) => string
  caption: string
  loadingLabel: string
  emptyTitle: string
  empty?: ReactNode
  onReload: () => void
  onMore: () => void
}

export function CursorTable<T>({ state, columns, rowKey, caption, loadingLabel, emptyTitle, empty, onReload, onMore }: Props<T>) {
  if (state.status === 'loading') {
    return <TableSkeleton label={loadingLabel} columns={columns.map((c) => (typeof c.header === 'string' ? c.header : ''))} />
  }
  if (state.status === 'failed') return <FailureAlert failure={state.failure} onRetry={onReload} />
  if (state.items.length === 0) return <EmptyState title={emptyTitle}>{empty}</EmptyState>
  return (
    <>
      <DataTable columns={columns} rows={state.items} rowKey={rowKey} caption={caption} />
      <WorkflowAlert failure={state.moreFailure} title="Gagal memuat halaman berikutnya" />
      {state.next !== null && (
        <div className="mt-3">
          <Button variant="outline-secondary" pending={state.more} onClick={onMore}>
            Muat lebih banyak
          </Button>
        </div>
      )}
    </>
  )
}
