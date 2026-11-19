import { useCallback, useEffect, useRef, useState } from 'react'
import { Link, useParams } from 'react-router'
import type { ApiFailure, ApiResult } from '../../api/client'
import { listDrafts, listMigrations } from '../../api/endpoints'
import type { DatabaseSummary, Draft, Execution, ID, List } from '../../api/types'
import { AccessBadge } from '../../components/AccessBadge'
import { DatabaseGate } from '../../components/DatabaseGate'
import { DataTable, type DataColumn } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { FailureAlert } from '../../components/FailureAlert'
import { Page } from '../../components/Page'
import { Panel } from '../../components/Panel'
import { StatusBadge } from '../../components/StatusBadge'
import { formatDateTime } from '../../lib/format'
import { paths } from '../../lib/paths'
import { TableSkeleton } from '../../components/Skeleton'

type ListState<T> =
  | { status: 'loading' }
  | { status: 'ready'; items: T[] }
  | { status: 'failed'; failure: ApiFailure }

function useList<T>(load: () => Promise<ApiResult<List<T>>>) {
  const [state, setState] = useState<ListState<T>>({ status: 'loading' })

  const seq = useRef(0)

  const fetchList = useCallback(async () => {
    const mine = ++seq.current
    const res = await load()
    if (mine !== seq.current) return
    setState(res.ok ? { status: 'ready', items: res.data.items } : { status: 'failed', failure: res })
  }, [load])

  const reload = useCallback(async () => {
    setState({ status: 'loading' })
    await fetchList()
  }, [fetchList])

  useEffect(() => {
    void fetchList()
    const ref = seq
    return () => {
      ref.current++
    }
  }, [fetchList])

  return { state, reload }
}

function historyColumns(databaseId: ID): DataColumn<Execution>[] {
  return [
    {
      key: 'version',
      header: 'Version',
      render: (x) => (
        <Link to={paths.execution(databaseId, x.id)}>
          <span className="mono">v{x.version}</span>
        </Link>
      ),
    },
    { key: 'title', header: 'Title', wrap: true, render: (x) => x.title },
    { key: 'by', header: 'Dijalankan oleh', render: (x) => x.executed_by.username },
    { key: 'started', header: 'Mulai', render: (x) => formatDateTime(x.started_at) },
    { key: 'finished', header: 'Selesai', render: (x) => formatDateTime(x.finished_at) },
    { key: 'status', header: 'Status', render: (x) => <StatusBadge status={x.status} /> },
  ]
}

function draftColumns(databaseId: ID): DataColumn<Draft>[] {
  return [
    {
      key: 'title',
      header: 'Title',
      wrap: true,
      render: (d) => <Link to={paths.draft(databaseId, d.id)}>{d.title}</Link>,
    },
    { key: 'revision', header: 'Revision', render: (d) => d.revision },
    { key: 'updated', header: 'Diubah', render: (d) => formatDateTime(d.updated_at) },
    {
      key: 'state',
      header: 'Keadaan',
      render: (d) =>
        d.applied_execution_id === null ? (
          <span className="badge bg-secondary-lt">Draft</span>
        ) : (
          <span className="badge bg-green-lt">Applied</span>
        ),
    },
  ]
}

function History({ databaseId }: { databaseId: ID }) {
  const load = useCallback(() => listMigrations(databaseId), [databaseId])
  const { state, reload } = useList(load)

  return (
    <Panel title="Riwayat migration">
      {state.status === 'loading' && (
        <TableSkeleton
          label="Memuat riwayat migration…"
          columns={['Version', 'Title', 'Dijalankan oleh', 'Mulai', 'Selesai', 'Status']}
        />
      )}
      {state.status === 'failed' && <FailureAlert failure={state.failure} onRetry={() => void reload()} />}
      {state.status === 'ready' &&
        (state.items.length === 0 ? (
          <EmptyState title="Belum ada migration">Belum ada migration yang dijalankan pada database ini.</EmptyState>
        ) : (
          <DataTable
            columns={historyColumns(databaseId)}
            rows={state.items}
            rowKey={(x) => x.id}
            caption="Riwayat migration"
          />
        ))}
    </Panel>
  )
}

function Drafts({ databaseId }: { databaseId: ID }) {
  const load = useCallback(() => listDrafts(databaseId), [databaseId])
  const { state, reload } = useList(load)

  return (
    <Panel title="Draft migration">
      {state.status === 'loading' && (
        <TableSkeleton label="Memuat draft…" columns={['Title', 'Revision', 'Diubah', 'Keadaan']} rows={3} />
      )}
      {state.status === 'failed' && <FailureAlert failure={state.failure} onRetry={() => void reload()} />}
      {state.status === 'ready' &&
        (state.items.length === 0 ? (
          <EmptyState title="Belum ada draft">Buat draft lewat tombol "Migration baru".</EmptyState>
        ) : (
          <DataTable columns={draftColumns(databaseId)} rows={state.items} rowKey={(d) => d.id} caption="Draft migration" />
        ))}
    </Panel>
  )
}

function DatabaseMigrations({ database }: { database: DatabaseSummary }) {
  const canEdit = database.access === 'edit'
  return (
    <>
      <div className="d-flex flex-wrap align-items-center gap-2 mb-3">
        <h2 className="h2 mb-0 me-1 text-break">{database.name}</h2>
        <AccessBadge access={database.access} />
        <span className="ms-auto d-flex flex-wrap align-items-center gap-3">
          <Link to={paths.database(database.id)}>Schema &amp; data</Link>
          <Link to={paths.proposals(database.id)}>Proposal</Link>
          <Link to={paths.jobs(database.id)}>Job</Link>
          <Link to={paths.databases}>Kembali ke daftar database</Link>
          {canEdit && (
            <Link className="btn btn-primary" to={paths.newDraft(database.id)}>
              Migration baru
            </Link>
          )}
        </span>
      </div>
      {canEdit && <Drafts databaseId={database.id} />}
      <History databaseId={database.id} />
    </>
  )
}

export function MigrationsPage() {
  const { databaseId = '' } = useParams()
  return (
    <Page title="Migrations">
      <DatabaseGate key={databaseId} databaseId={databaseId}>
        {(database) => <DatabaseMigrations database={database} />}
      </DatabaseGate>
    </Page>
  )
}
