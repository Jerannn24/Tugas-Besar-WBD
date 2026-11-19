import { useCallback } from 'react'
import { Link } from 'react-router'
import type { DatabaseSummary, ID, Proposal } from '../../api/types'
import { listProposals } from '../../api/workflow'
import { Button } from '../../components/Button'
import { CursorTable } from '../../components/CursorTable'
import type { DataColumn } from '../../components/DataTable'
import { Panel } from '../../components/Panel'
import { ShortId } from '../../components/ShortId'
import { ProposalBadge } from '../../components/StateBadge'
import { useCursorList } from '../../lib/cursor'
import { paths } from '../../lib/paths'
import { ACTION_LABELS, validationLabel } from './labels'

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
  const loader = useCallback((cursor: string | null) => listProposals(database.id, { cursor }), [database.id])
  const { state, reload, loadMore } = useCursorList(loader)

  return (
    <Panel
      title="Proposal migration"
      actions={
        <Button variant="outline-secondary" disabled={state.status === 'loading'} onClick={() => void reload()}>
          Muat ulang
        </Button>
      }
    >
      <CursorTable
        state={state}
        columns={columns(database.id)}
        rowKey={(p) => p.proposal_id}
        caption="Proposal migration"
        loadingLabel="Memuat proposal…"
        emptyTitle="Belum ada proposal"
        empty='Proposal dibuat dari halaman draft lewat tombol "Ajukan proposal".'
        onReload={() => void reload()}
        onMore={() => void loadMore()}
      />
    </Panel>
  )
}
