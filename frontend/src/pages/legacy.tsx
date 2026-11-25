import type { ReactNode } from 'react'
import { Link, Navigate, useSearchParams } from 'react-router'
import { Alert } from '../components/Alert'
import { Page } from '../components/Page'
import { paths } from '../lib/paths'

// Redirect URL M1 ke route SPA. 
function param(params: URLSearchParams, name: string): string | null {
  const value = params.get(name)
  return value === null || value.trim() === '' ? null : value
}

function MissingDatabase({ title }: { title: string }) {
  return (
    <Page title={title}>
      <Alert variant="warning" title="Database belum dipilih">
        <p className="mb-2">
          Parameter <code>database</code> tidak ada di alamat halaman.
        </p>
        <Link to={paths.databases}>Kembali ke daftar database</Link>
      </Alert>
    </Page>
  )
}

function Invalid({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Page title={title}>
      <Alert variant="danger" title="Alamat halaman tidak valid">
        {children}
      </Alert>
    </Page>
  )
}

/** `/database/?id=X[&schema=&table=]` */
export function LegacyDatabase() {
  const [params] = useSearchParams()
  const id = param(params, 'id')
  if (id === null) {
    return (
      <Page title="Database">
        <Alert variant="warning" title="Database belum dipilih">
          <p className="mb-2">
            Parameter <code>id</code> tidak ada di alamat halaman.
          </p>
          <Link to={paths.databases}>Kembali ke daftar database</Link>
        </Alert>
      </Page>
    )
  }
  const schema = param(params, 'schema')
  const table = param(params, 'table')
  const to = schema !== null && table !== null ? paths.database(id, { schema, table }) : paths.database(id)
  return <Navigate to={to} replace />
}

/** `/migrations/?database=X` */
export function LegacyMigrations() {
  const [params] = useSearchParams()
  const id = param(params, 'database')
  if (id === null) return <MissingDatabase title="Migrations" />
  return <Navigate to={paths.migrations(id)} replace />
}

/** `/migration/?database=X[&draft=Y | &execution=Z]` */
export function LegacyMigration() {
  const [params] = useSearchParams()
  const id = param(params, 'database')
  const draftId = param(params, 'draft')
  const executionId = param(params, 'execution')
  if (id === null) return <MissingDatabase title="Migration" />
  if (draftId !== null && executionId !== null) {
    return (
      <Invalid title="Migration">
        <p className="mb-2">
          Parameter <code>draft</code> dan <code>execution</code> tidak boleh dipakai bersamaan.
        </p>
        <Link to={paths.migrations(id)}>Kembali ke riwayat migration</Link>
      </Invalid>
    )
  }
  const to =
    draftId !== null
      ? paths.draft(id, draftId)
      : executionId !== null
        ? paths.execution(id, executionId)
        : paths.newDraft(id)
  return <Navigate to={to} replace />
}
