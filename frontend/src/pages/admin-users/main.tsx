import { useState } from 'react'
import { listUsers } from '../../api/endpoints'
import type { SessionView, User } from '../../api/types'
import { Button } from '../../components/Button'
import { DataTable, type DataColumn } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { FailureAlert } from '../../components/FailureAlert'
import { Layout } from '../../components/Layout'
import { Loading } from '../../components/Loading'
import { Panel } from '../../components/Panel'
import { ADMIN_FORBIDDEN, useLoad } from '../../lib/load'
import { mountPage } from '../../lib/mount'
import { CreateUser } from './CreateUser'
import { EditUser } from './EditUser'

function UsersPage({ session }: { session: SessionView }) {
  const { state, reload, update } = useLoad(listUsers)
  const [selectedId, setSelectedId] = useState<string | null>(null)

  if (state.status === 'loading') return <Loading label="Memuat daftar user…" />
  if (state.status === 'failed') {
    return <FailureAlert failure={state.failure} forbiddenText={ADMIN_FORBIDDEN} onRetry={() => void reload()} />
  }

  const users = state.data.items
  const selected = users.find((u) => u.id === selectedId) ?? null

  const columns: DataColumn<User>[] = [
    {
      key: 'username',
      header: 'Username',
      render: (u) => (
        <>
          <span className="mono">{u.username}</span>
          {u.id === session.user.id && <span className="badge bg-secondary-lt ms-2">Anda</span>}
        </>
      ),
    },
    {
      key: 'admin',
      header: 'Admin',
      render: (u) => (u.is_admin ? <span className="badge bg-blue-lt">Platform Admin</span> : <span className="text-secondary">—</span>),
    },
    {
      key: 'active',
      header: 'Status',
      render: (u) =>
        u.is_active ? <span className="badge bg-green-lt">Aktif</span> : <span className="badge bg-red-lt">Nonaktif</span>,
    },
    {
      key: 'actions',
      header: 'Aksi',
      render: (u) => (
        <Button
          variant="outline-secondary"
          className="btn-sm"
          aria-pressed={u.id === selectedId}
          onClick={() => setSelectedId(u.id)}
        >
          Edit
        </Button>
      ),
    },
  ]

  return (
    <div className="admin-layout">
      <Panel title="Daftar user">
        {users.length === 0 ? (
          <EmptyState title="Belum ada user" />
        ) : (
          <DataTable columns={columns} rows={users} rowKey={(u) => u.id} caption="Daftar user platform" />
        )}
        <p className="text-secondary mt-3 mb-0">
          User tidak dapat dihapus; nonaktifkan lewat Edit. Akses database diatur lewat{' '}
          <a href="/admin/groups/">group</a>.
        </p>
      </Panel>
      <div>
        {selected ? (
          <EditUser
            key={selected.id}
            user={selected}
            isSelf={selected.id === session.user.id}
            onSaved={(saved) => update((d) => ({ items: d.items.map((u) => (u.id === saved.id ? saved : u)) }))}
            onClose={() => setSelectedId(null)}
          />
        ) : (
          <CreateUser onCreated={(user) => update((d) => ({ items: [...d.items, user] }))} />
        )}
      </div>
    </div>
  )
}

mountPage(
  <Layout activePage="admin-users" title="Admin: Users">
    {(session) => <UsersPage session={session} />}
  </Layout>,
)
