import type { DatabaseSummary } from '../../api/types'
import { AccessBadge } from '../../components/AccessBadge'
import { Alert } from '../../components/Alert'
import { DatabaseGate } from '../../components/DatabaseGate'
import { Layout } from '../../components/Layout'
import { mountPage } from '../../lib/mount'
import { getQueryParam, pageUrl } from '../../lib/query'
import { CreateDraft } from './CreateDraft'
import { DraftEditor } from './DraftEditor'
import { ExecutionView } from './ExecutionView'

type Mode = { kind: 'create' } | { kind: 'draft'; draftId: string } | { kind: 'execution'; executionId: string }

function Header({ database }: { database: DatabaseSummary }) {
  return (
    <div className="d-flex flex-wrap align-items-center gap-2 mb-3">
      <h2 className="h2 mb-0 me-1 text-break">{database.name}</h2>
      <AccessBadge access={database.access} />
      <span className="ms-auto">
        <a href={pageUrl('/migrations/', { database: database.id })}>Riwayat migration</a>
      </span>
    </div>
  )
}

function MigrationPage({ database, mode }: { database: DatabaseSummary; mode: Mode }) {
  if (mode.kind !== 'execution' && database.access !== 'edit') {
    return (
      <>
        <Header database={database} />
        <Alert variant="danger" title="Akses ditolak">
          <p className="mb-2">Anda tidak memiliki akses untuk membuat atau mengubah migration pada database ini.</p>
          <a href={pageUrl('/migrations/', { database: database.id })}>Lihat riwayat migration</a>
        </Alert>
      </>
    )
  }

  return (
    <>
      <Header database={database} />
      {mode.kind === 'create' && <CreateDraft database={database} />}
      {mode.kind === 'draft' && <DraftEditor database={database} draftId={mode.draftId} />}
      {mode.kind === 'execution' && <ExecutionView database={database} executionId={mode.executionId} />}
    </>
  )
}

function MigrationRoute() {
  const databaseId = getQueryParam('database')
  const draftId = getQueryParam('draft')
  const executionId = getQueryParam('execution')

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
  if (draftId !== null && executionId !== null) {
    return (
      <Alert variant="danger" title="Alamat halaman tidak valid">
        <p className="mb-2">
          Parameter <code>draft</code> dan <code>execution</code> tidak boleh dipakai bersamaan.
        </p>
        <a href={pageUrl('/migrations/', { database: databaseId })}>Kembali ke riwayat migration</a>
      </Alert>
    )
  }

  const mode: Mode =
    draftId !== null
      ? { kind: 'draft', draftId }
      : executionId !== null
        ? { kind: 'execution', executionId }
        : { kind: 'create' }

  return (
    <DatabaseGate databaseId={databaseId}>{(database) => <MigrationPage database={database} mode={mode} />}</DatabaseGate>
  )
}

const title = getQueryParam('execution') ? 'Detail execution' : getQueryParam('draft') ? 'Draft migration' : 'Migration baru'

mountPage(
  <Layout activePage="migration" title={title}>
    <MigrationRoute />
  </Layout>,
)
