import { useCallback, useEffect, useRef, useState } from 'react'
import { Link } from 'react-router'
import type { ApiFailure } from '../../api/client'
import type { DatabaseSummary, ID, Proposal } from '../../api/types'
import { listProposals } from '../../api/workflow'
import { Button } from '../../components/Button'
import { DataTable, type DataColumn } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { FailureAlert } from '../../components/FailureAlert'
import { Panel } from '../../components/Panel'
import { ShortId } from '../../components/ShortId'
import { TableSkeleton } from '../../components/Skeleton'
import { ProposalBadge } from '../../components/StateBadge'
import { WorkflowAlert } from '../../components/WorkflowAlert'
import { paths } from '../../lib/paths'
import { ACTION_LABELS, validationLabel } from './labels'

type ListState =
  | { status: 'loading' }
  | { status: 'failed'; failure: ApiFailure }
  | { status: 'ready'; items: Proposal[]; next: string | null; more: 'idle' | 'loading'; moreFailure: ApiFailure | null }

const HEADERS = ['Proposal', 'Title', 'Aksi', 'Status', 'Revision', 'Validasi']

function columns(databaseId: ID): DataColumn<Proposal>[] {
  return [
    {
      key: 'id',
      header: 'Proposal',
      render: (p) => (
        <Link to={paths.proposal(databaseId, p.proposal_id)}>
          <ShortId id={p.proposal_id} />
        </Link>
      ),
    },
    { key: 'title', header: 'Title', wrap: true, render: (p) => p.snapshot.title },
    { key: 'action', header: 'Aksi', render: (p) => ACTION_LABELS[p.action] ?? p.action },
    { key: 'status', header: 'Status', render: (p) => <ProposalBadge status={p.status} /> },
    { key: 'revision', header: 'Revision', render: (p) => p.revision },
    { key: 'validation', header: 'Validasi', render: (p) => validationLabel(p.validation) },
  ]
}

export function ProposalList({ database }: { database: DatabaseSummary }) {
  const [state, setState] = useState<ListState>({ status: 'loading' })
  const seq = useRef(0)

  const fetchFirst = useCallback(async () => {
    const mine = ++seq.current
    const res = await listProposals(database.id)
    if (mine !== seq.current) return
    setState(
      res.ok
        ? { status: 'ready', items: res.data.items, next: res.data.next_cursor, more: 'idle', moreFailure: null }
        : { status: 'failed', failure: res },
    )
  }, [database.id])

  const loadFirst = useCallback(() => {
    setState({ status: 'loading' })
    return fetchFirst()
  }, [fetchFirst])

  useEffect(() => {
    void fetchFirst()
    const ref = seq
    return () => {
      ref.current++
    }
  }, [fetchFirst])

  async function loadMore() {
    if (state.status !== 'ready' || state.next === null || state.more === 'loading') return
    const mine = ++seq.current
    setState({ ...state, more: 'loading', moreFailure: null })
    const res = await listProposals(database.id, { cursor: state.next })
    if (mine !== seq.current) return
    if (res.ok) {
      setState({
        status: 'ready',
        items: [...state.items, ...res.data.items],
        next: res.data.next_cursor,
        more: 'idle',
        moreFailure: null,
      })
    } else if (res.kind === 'http' && res.status === 400) {
      void loadFirst()
    } else {
      setState({ ...state, more: 'idle', moreFailure: res })
    }
  }

  return (
    <Panel
      title="Proposal migration"
      actions={
        <Button variant="outline-secondary" disabled={state.status === 'loading'} onClick={() => void loadFirst()}>
          Muat ulang
        </Button>
      }
    >
      {state.status === 'loading' && <TableSkeleton label="Memuat proposal…" columns={HEADERS} />}
      {state.status === 'failed' && <FailureAlert failure={state.failure} onRetry={() => void loadFirst()} />}
      {state.status === 'ready' &&
        (state.items.length === 0 ? (
          <EmptyState title="Belum ada proposal">
            Proposal dibuat dari halaman draft lewat tombol "Ajukan proposal".
          </EmptyState>
        ) : (
          <>
            <DataTable
              columns={columns(database.id)}
              rows={state.items}
              rowKey={(p) => p.proposal_id}
              caption="Proposal migration"
            />
            <WorkflowAlert failure={state.moreFailure} title="Gagal memuat halaman berikutnya" />
            {state.next !== null && (
              <div className="mt-3">
                <Button variant="outline-secondary" pending={state.more === 'loading'} onClick={() => void loadMore()}>
                  Muat lebih banyak
                </Button>
              </div>
            )}
          </>
        ))}
    </Panel>
  )
}
