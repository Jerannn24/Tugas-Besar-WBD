import { useCallback } from 'react'
import { Link } from 'react-router'
import { listJobs } from '../../api/jobs'
import type { DatabaseSummary, ID, JobView } from '../../api/types'
import { Button } from '../../components/Button'
import { CursorTable } from '../../components/CursorTable'
import type { DataColumn } from '../../components/DataTable'
import { Panel } from '../../components/Panel'
import { ShortId } from '../../components/ShortId'
import { JobBadge } from '../../components/StateBadge'
import { useCursorList } from '../../lib/cursor'
import { formatDateTime } from '../../lib/format'
import { paths } from '../../lib/paths'
import { ACTION_LABELS } from '../proposals/labels'

function columns(databaseId: ID): DataColumn<JobView>[] {
  return [
    {
      key: 'id',
      header: 'Job',
      render: (j) => (
        <Link to={paths.job(databaseId, j.job_id)}>
          <ShortId id={j.job_id} />
        </Link>
      ),
    },
    { key: 'action', header: 'Aksi', render: (j) => `${ACTION_LABELS[j.action] ?? j.action} · ${j.mode}` },
    { key: 'status', header: 'Status', render: (j) => <JobBadge status={j.status} /> },
    {
      key: 'execution',
      header: 'Hasil execution',
      render: (j) => (j.execution_status !== j.status ? <JobBadge status={j.execution_status} /> : '—'),
    },
    { key: 'attempt', header: 'Attempt', render: (j) => j.attempt_no },
    {
      key: 'block',
      header: 'Database',
      render: (j) => (j.blocks_database ? <span className="badge bg-orange-lt">Tertahan</span> : '—'),
    },
    { key: 'created', header: 'Dibuat', render: (j) => formatDateTime(j.created_at) },
  ]
}

export function JobList({ database }: { database: DatabaseSummary }) {
  const loader = useCallback((cursor: string | null) => listJobs(database.id, { cursor }), [database.id])
  const { state, reload, loadMore } = useCursorList(loader)

  return (
    <Panel
      title="Job migration"
      actions={
        <Button variant="outline-secondary" disabled={state.status === 'loading'} onClick={() => void reload()}>
          Muat ulang
        </Button>
      }
    >
      <CursorTable
        state={state}
        columns={columns(database.id)}
        rowKey={(j) => j.job_id}
        caption="Job migration"
        loadingLabel="Memuat job…"
        emptyTitle="Belum ada job"
        empty="Job dibuat saat proposal yang disetujui dimasukkan ke antrean."
        onReload={() => void reload()}
        onMore={() => void loadMore()}
      />
    </Panel>
  )
}
