import { useCallback, useEffect, useRef, useState } from 'react'
import type { ApiFailure, ApiResult } from '../../api/client'
import { listDrafts, listMigrations } from '../../api/endpoints'
import type { DatabaseSummary, Draft, Execution, ID, List } from '../../api/types'
import { AccessBadge } from '../../components/AccessBadge'
import { Alert } from '../../components/Alert'
import { DatabaseGate } from '../../components/DatabaseGate'
import { DataTable, type DataColumn } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { FailureAlert } from '../../components/FailureAlert'
import { Layout } from '../../components/Layout'
import { Loading } from '../../components/Loading'
import { Panel } from '../../components/Panel'
import { StatusBadge } from '../../components/StatusBadge'
import { formatDateTime } from '../../lib/format'
import { mountPage } from '../../lib/mount'
import { getQueryParam, pageUrl } from '../../lib/query'

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
        <a href={pageUrl('/migration/', { database: databaseId, execution: x.id })}>
          <span className="mono">v{x.version}</span>
        </a>
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
      render: (d) => <a href={pageUrl('/migration/', { database: databaseId, draft: d.id })}>{d.title}</a>,
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
      {state.status === 'loading' && <Loading label="Memuat riwayat migration…" />}
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
      {state.status === 'loading' && <Loading label="Memuat draft…" />}
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

function MigrationsPage({ database }: { database: DatabaseSummary }) {
  const canEdit = database.access === 'edit'
  return (
    <>
      <div className="d-flex flex-wrap align-items-center gap-2 mb-3">
        <h2 className="h2 mb-0 me-1 text-break">{database.name}</h2>
        <AccessBadge access={database.access} />
        <span className="ms-auto d-flex flex-wrap align-items-center gap-3">
          <a href={pageUrl('/database/', { id: database.id })}>Schema &amp; data</a>
          <a href="/databases/">Kembali ke daftar database</a>
          {canEdit && (
            <a className="btn btn-primary" href={pageUrl('/migration/', { database: database.id })}>
              Migration baru
            </a>
          )}
        </span>
      </div>
      {canEdit && <Drafts databaseId={database.id} />}
      <History databaseId={database.id} />
    </>
  )
}

function MigrationsRoute() {
  const databaseId = getQueryParam('database')
  if (databaseId === null) {
    return (
      <Alert variant="warning" title="Database belum dipilih">
        <p className="mb-2">
          Parameter <code>database</code> tidak ada di alamat halaman.
        </p>
        <a href="/databases/">Kembali ke daftar database</a>
      </Alert>
    )
  }
  return <DatabaseGate databaseId={databaseId}>{(database) => <MigrationsPage database={database} />}</DatabaseGate>
}

mountPage(
  <Layout activePage="migrations" title="Migrations">
    <MigrationsRoute />
  </Layout>,
)
