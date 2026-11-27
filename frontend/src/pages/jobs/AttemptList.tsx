import { useCallback } from 'react'
import { Link } from 'react-router'
import { listAttempts } from '../../api/jobs'
import type { Attempt, DatabaseSummary, ID } from '../../api/types'
import { Button } from '../../components/Button'
import { CursorTable } from '../../components/CursorTable'
import type { DataColumn } from '../../components/DataTable'
import { Panel } from '../../components/Panel'
import { ShortId } from '../../components/ShortId'
import { AttemptBadge } from '../../components/StateBadge'
import { useCursorList } from '../../lib/cursor'
import { formatDateTime } from '../../lib/format'
import { paths } from '../../lib/paths'

function JobCell({ databaseId, attempt }: { databaseId: ID; attempt: Attempt }) {
  if (attempt.job_id !== null) {
    return (
      <Link to={paths.job(databaseId, attempt.job_id)}>
        <ShortId id={attempt.job_id} />
      </Link>
    )
  }
  return (
    <span className="d-inline-flex flex-wrap align-items-center gap-1">
      <span className="badge bg-secondary-lt">Legacy M1</span>
      {attempt.legacy_execution_id && (
        <Link to={paths.execution(databaseId, attempt.legacy_execution_id)}>
          <ShortId id={attempt.legacy_execution_id} />
        </Link>
      )}
    </span>
  )
}

function columns(databaseId: ID): DataColumn<Attempt>[] {
  return [
    { key: 'id', header: 'Attempt', render: (a) => <ShortId id={a.attempt_id} /> },
    { key: 'job', header: 'Job', render: (a) => <JobCell databaseId={databaseId} attempt={a} /> },
    { key: 'execution', header: 'Execution', render: (a) => <ShortId id={a.execution_id} /> },
    { key: 'no', header: 'No.', render: (a) => a.attempt_no },
    { key: 'status', header: 'Status', render: (a) => <AttemptBadge status={a.status} /> },
    { key: 'started', header: 'Mulai', render: (a) => formatDateTime(a.started_at) },
    { key: 'finished', header: 'Selesai', render: (a) => formatDateTime(a.finished_at) },
    {
      key: 'error',
      header: 'Error',
      wrap: true,
      render: (a) =>
        a.error ? (
          <>
            <code>{a.error.code}</code>: {a.error.message}
          </>
        ) : (
          '—'
        ),
    },
  ]
}

export function AttemptList({ database }: { database: DatabaseSummary }) {
  const loader = useCallback((cursor: string | null) => listAttempts(database.id, { cursor }), [database.id])
  const { state, reload, loadMore } = useCursorList(loader)

  return (
    <Panel
      title="Attempt eksekusi"
      actions={
        <Button variant="outline-secondary" disabled={state.status === 'loading'} onClick={() => void reload()}>
          Muat ulang
        </Button>
      }
    >
      <p className="text-secondary">
        Pelengkap riwayat migration: satu baris per percobaan eksekusi, termasuk data lama M1 tanpa job.
      </p>
      <CursorTable
        state={state}
        columns={columns(database.id)}
        rowKey={(a) => a.attempt_id}
        caption="Attempt eksekusi"
        loadingLabel="Memuat attempt…"
        emptyTitle="Belum ada attempt"
        onReload={() => void reload()}
        onMore={() => void loadMore()}
      />
    </Panel>
  )
}
