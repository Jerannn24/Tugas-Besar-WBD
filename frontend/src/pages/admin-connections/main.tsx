import { useState } from 'react'
import { errorMessage } from '../../api/client'
import { deleteConnection, listConnections, testConnection } from '../../api/endpoints'
import type { ConnectionSummary, ID } from '../../api/types'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { DataTable, type DataColumn } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { FailureAlert } from '../../components/FailureAlert'
import { Layout } from '../../components/Layout'
import { Loading } from '../../components/Loading'
import { MutationAlert } from '../../components/MutationAlert'
import { Panel } from '../../components/Panel'
import { ADMIN_FORBIDDEN, useLoad } from '../../lib/load'
import { mountPage } from '../../lib/mount'
import { describeFailure, type MutationFailure } from '../../lib/mutation'
import { ConnectionForm } from './ConnectionForm'

type TestResult = { status: 'pending' } | { status: 'reachable' } | { status: 'failed'; text: string }

function TestStatus({ result }: { result: TestResult | undefined }) {
  if (!result) return <span className="text-secondary">Belum dites</span>
  switch (result.status) {
    case 'pending':
      return <span className="text-secondary">Menguji…</span>
    case 'reachable':
      return <span className="badge bg-green-lt">Dapat dihubungi</span>
    case 'failed':
      return <span className="text-danger cell-wrap">{result.text}</span>
  }
}

function ExecutionStatus({ connection: c }: { connection: ConnectionSummary }) {
  if (c.execution_ready === undefined) return <span className="text-secondary">—</span>
  if (!c.execution_ready) return <span className="badge bg-secondary-lt">View only</span>
  const roles = [c.executor_username && `executor: ${c.executor_username}`, c.preparer_username && `preparer: ${c.preparer_username}`]
  return (
    <>
      <span className="badge bg-green-lt">Siap eksekusi</span>
      {roles.some(Boolean) && <div className="small text-secondary mono">{roles.filter(Boolean).join(', ')}</div>}
    </>
  )
}

function ConnectionsPage() {
  const { state, reload, update } = useLoad(listConnections)
  const [editingId, setEditingId] = useState<ID | null>(null)
  const [tests, setTests] = useState<Record<ID, TestResult>>({})
  const [deleting, setDeleting] = useState<ConnectionSummary | null>(null)
  const [deletePending, setDeletePending] = useState(false)
  const [failure, setFailure] = useState<MutationFailure | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  if (state.status === 'loading') return <Loading label="Memuat daftar connection…" />
  if (state.status === 'failed') {
    return <FailureAlert failure={state.failure} forbiddenText={ADMIN_FORBIDDEN} onRetry={() => void reload()} />
  }

  const connections = state.data.items
  const editing = connections.find((c) => c.id === editingId) ?? null

  async function runTest(c: ConnectionSummary) {
    if (tests[c.id]?.status === 'pending') return
    setTests((t) => ({ ...t, [c.id]: { status: 'pending' } }))
    const res = await testConnection(c.id)
    let result: TestResult
    if (res.ok) result = { status: 'reachable' }
    else if (res.kind === 'http' && res.error.code === 'TARGET_UNAVAILABLE') {
      result = { status: 'failed', text: `Tidak dapat dihubungi: ${res.error.message}` }
    } else result = { status: 'failed', text: errorMessage(res) }
    setTests((t) => ({ ...t, [c.id]: result }))
  }

  async function confirmDelete() {
    if (!deleting || deletePending) return
    const target = deleting
    setDeletePending(true)
    setFailure(null)
    setNotice(null)
    const res = await deleteConnection(target.id)
    setDeletePending(false)
    setDeleting(null)
    if (res.ok) {
      update((d) => ({ items: d.items.filter((c) => c.id !== target.id) }))
      if (editingId === target.id) setEditingId(null)
      setNotice(`Connection ${target.name} dihapus.`)
      return
    }
    const described = describeFailure(res, 'Connection gagal dihapus')
    if (described && res.kind === 'http' && res.error.code === 'DATABASE_BUSY') {
      described.text = `${target.name} sedang menjalankan migration (running/unknown). Tunggu hingga statusnya selesai lalu coba lagi.`
    }
    setFailure(described)
  }

  const columns: DataColumn<ConnectionSummary>[] = [
    { key: 'name', header: 'Nama', render: (c) => <strong>{c.name}</strong> },
    { key: 'endpoint', header: 'Host:port', render: (c) => <span className="mono">{c.host}:{c.port}</span> },
    { key: 'database', header: 'Database', render: (c) => <span className="mono">{c.database_name}</span> },
    { key: 'username', header: 'Username', render: (c) => <span className="mono">{c.username}</span> },
    { key: 'ssl', header: 'SSL', render: (c) => c.ssl_mode },
    { key: 'execution', header: 'Eksekusi', wrap: true, render: (c) => <ExecutionStatus connection={c} /> },
    { key: 'test', header: 'Test', wrap: true, render: (c) => <TestStatus result={tests[c.id]} /> },
    {
      key: 'actions',
      header: 'Aksi',
      render: (c) => (
        <div className="d-flex gap-2">
          <Button variant="outline-secondary" className="btn-sm" aria-pressed={c.id === editingId} onClick={() => setEditingId(c.id)}>
            Edit
          </Button>
          <Button
            variant="outline-secondary"
            className="btn-sm"
            pending={tests[c.id]?.status === 'pending'}
            onClick={() => void runTest(c)}
          >
            Test
          </Button>
          <Button variant="danger" className="btn-sm" onClick={() => setDeleting(c)}>
            Hapus
          </Button>
        </div>
      ),
    },
  ]

  return (
    <>
      <MutationAlert failure={failure} />
      {notice && <Alert variant="success">{notice}</Alert>}
      <Panel title="Connection database target">
        {connections.length === 0 ? (
          <EmptyState title="Belum ada connection">Tambahkan connection PostgreSQL target dengan form di bawah.</EmptyState>
        ) : (
          <DataTable columns={columns} rows={connections} rowKey={(c) => c.id} caption="Daftar connection database target" />
        )}
        <p className="text-secondary mt-3 mb-0">
          Password (termasuk kredensial eksekusi) tidak pernah dikirim server ke browser. Akses user ke database diatur lewat grant di{' '}
          <a href="/admin/groups/">Admin: Groups</a>.
        </p>
      </Panel>
      {editing ? (
        <ConnectionForm
          key={editing.id}
          connection={editing}
          onSaved={(saved) => update((d) => ({ items: d.items.map((c) => (c.id === saved.id ? saved : c)) }))}
          onClose={() => setEditingId(null)}
        />
      ) : (
        <ConnectionForm connection={null} onSaved={(saved) => update((d) => ({ items: [...d.items, saved] }))} />
      )}

      <ConfirmDialog
        open={deleting !== null}
        title="Hapus connection?"
        danger
        confirmLabel="Hapus"
        pending={deletePending}
        onConfirm={() => void confirmDelete()}
        onCancel={() => setDeleting(null)}
      >
        <p>
          Connection <strong>{deleting?.name}</strong> akan dihapus. Semua grant group untuk database ini dicabut dan
          session user terdampak diakhiri.
        </p>
        <p className="mb-0 text-secondary">
          Riwayat migration tetap tersimpan. Penghapusan ditolak jika target sedang menjalankan migration.
        </p>
      </ConfirmDialog>
    </>
  )
}

mountPage(
  <Layout activePage="admin-connections" title="Admin: Connections">
    <ConnectionsPage />
  </Layout>,
)
