import { useCallback, useEffect, useState } from 'react'
import { errorMessage, type ApiFailure } from '../../api/client'
import { listDatabases } from '../../api/endpoints'
import type { DatabaseSummary } from '../../api/types'
import { AccessBadge } from '../../components/AccessBadge'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { DataTable, type DataColumn } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { Layout } from '../../components/Layout'
import { Loading } from '../../components/Loading'
import { Panel } from '../../components/Panel'
import { mountPage } from '../../lib/mount'
import { pageUrl } from '../../lib/query'

type ListState =
  | { status: 'loading' }
  | { status: 'ready'; items: DatabaseSummary[] }
  | { status: 'forbidden' }
  | { status: 'expired' }
  | { status: 'error'; failure: ApiFailure }

const COLUMNS: DataColumn<DatabaseSummary>[] = [
  { key: 'name', header: 'Nama', render: (d) => <strong>{d.name}</strong> },
  { key: 'access', header: 'Akses', render: (d) => <AccessBadge access={d.access} /> },
  {
    key: 'actions',
    header: 'Aksi',
    render: (d) => (
      <div className="d-flex gap-3">
        <a href={pageUrl('/database/', { id: d.id })}>Schema &amp; data</a>
        <a href={pageUrl('/migrations/', { database: d.id })}>Riwayat migration</a>
      </div>
    ),
  },
]

function DatabaseList({ isAdmin }: { isAdmin: boolean }) {
  const [state, setState] = useState<ListState>({ status: 'loading' })

  const load = useCallback(async () => {
    setState({ status: 'loading' })
    const res = await listDatabases()
    if (res.ok) setState({ status: 'ready', items: res.data.items })
    else if (res.kind === 'http' && res.status === 401) setState({ status: 'expired' })
    else if (res.kind === 'http' && res.status === 403) setState({ status: 'forbidden' })
    else setState({ status: 'error', failure: res })
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  switch (state.status) {
    case 'loading':
      return <Loading label="Memuat daftar database…" />
    case 'expired':
      return (
        <Alert variant="warning" title="Sesi berakhir">
          Mengalihkan ke <a href="/login/">halaman login</a>…
        </Alert>
      )
    case 'forbidden':
      return (
        <Alert variant="danger" title="Akses ditolak">
          Anda tidak memiliki akses.
        </Alert>
      )
    case 'error':
      return (
        <Alert variant="danger" title="Gagal memuat daftar database">
          <p className="mb-2">{errorMessage(state.failure)}</p>
          <Button variant="outline-secondary" onClick={() => void load()}>
            Muat ulang
          </Button>
        </Alert>
      )
    case 'ready':
      return (
        <Panel title="Database yang dapat diakses">
          {state.items.length === 0 ? (
            isAdmin ? (
              <EmptyState title="Belum ada database untuk akun ini">
                Admin platform tidak otomatis punya akses database; berikan grant lewat group di{' '}
                <a href="/admin/groups/">Admin: Groups</a>.
              </EmptyState>
            ) : (
              <EmptyState title="Belum ada database">
                Anda belum memiliki akses ke database mana pun.
              </EmptyState>
            )
          ) : (
            <DataTable
              columns={COLUMNS}
              rows={state.items}
              rowKey={(d) => d.id}
              caption="Daftar database dan level akses"
            />
          )}
        </Panel>
      )
  }
}

mountPage(
  <Layout activePage="databases" title="Databases">
    {(session) => <DatabaseList isAdmin={session.user.is_admin} />}
  </Layout>,
)
