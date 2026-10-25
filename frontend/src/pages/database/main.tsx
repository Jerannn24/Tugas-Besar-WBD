import { useCallback, useEffect, useRef, useState } from 'react'
import type { ApiFailure } from '../../api/client'
import { listSchemas } from '../../api/endpoints'
import type { DatabaseSummary, SchemaSummary } from '../../api/types'
import { AccessBadge } from '../../components/AccessBadge'
import { DatabaseGate } from '../../components/DatabaseGate'
import { Alert } from '../../components/Alert'
import { EmptyState } from '../../components/EmptyState'
import { Layout } from '../../components/Layout'
import { Loading } from '../../components/Loading'
import { Panel } from '../../components/Panel'
import { mountPage } from '../../lib/mount'
import { getQueryParam, pageUrl, replaceQueryParams } from '../../lib/query'
import { FailureAlert } from '../../components/FailureAlert'
import { SchemaSidebar, type Selection } from './SchemaSidebar'
import { TableView, type Tab } from './TableView'

type SchemasState =
  | { status: 'loading' }
  | { status: 'ready'; schemas: SchemaSummary[] }
  | { status: 'failed'; failure: ApiFailure }

function BackLink() {
  return <a href="/databases/">Kembali ke daftar database</a>
}

function initialSelection(): Selection | null {
  const schema = getQueryParam('schema')
  const table = getQueryParam('table')
  return schema !== null && table !== null ? { schema, table } : null
}

function Viewer({ database }: { database: DatabaseSummary }) {
  const [schemas, setSchemas] = useState<SchemasState>({ status: 'loading' })
  const [selection, setSelection] = useState<Selection | null>(initialSelection)
  const [tab, setTab] = useState<Tab>('columns')
  const schemasSeq = useRef(0)

  const loadSchemas = useCallback(async () => {
    const seq = ++schemasSeq.current
    setSchemas({ status: 'loading' })
    const res = await listSchemas(database.id)
    if (seq !== schemasSeq.current) return
    setSchemas(res.ok ? { status: 'ready', schemas: res.data.items } : { status: 'failed', failure: res })
  }, [database.id])

  useEffect(() => {
    void loadSchemas()
  }, [loadSchemas])

  function select(next: Selection) {
    setSelection(next)
    replaceQueryParams({ id: database.id, schema: next.schema, table: next.table })
  }

  return (
    <>
      <div className="d-flex flex-wrap align-items-center gap-2 mb-3">
        <h2 className="h2 mb-0 me-1 text-break">{database.name}</h2>
        <AccessBadge access={database.access} />
        <span className="ms-auto d-flex gap-3">
          <a href={pageUrl('/migrations/', { database: database.id })}>Riwayat migration</a>
          <BackLink />
        </span>
      </div>

      <div className="viewer-layout">
        <Panel title="Schema" className="mb-0">
          {schemas.status === 'loading' && <Loading label="Memuat schema…" />}
          {schemas.status === 'failed' && (
            <FailureAlert failure={schemas.failure} onRetry={() => void loadSchemas()} />
          )}
          {schemas.status === 'ready' && (
            <SchemaSidebar schemas={schemas.schemas} selected={selection} onSelect={select} />
          )}
        </Panel>

        <div style={{ minWidth: 0 }}>
          {selection ? (
            <TableView
              key={`${selection.schema}\u0000${selection.table}`}
              databaseId={database.id}
              selection={selection}
              tab={tab}
              onTabChange={setTab}
            />
          ) : (
            <Panel>
              <EmptyState title="Belum ada table dipilih">
                Pilih table di daftar schema untuk melihat column, constraint, index, dan data.
              </EmptyState>
            </Panel>
          )}
        </div>
      </div>
    </>
  )
}

function DatabaseRoute() {
  const databaseId = getQueryParam('id')
  if (databaseId === null) {
    return (
      <Alert variant="warning" title="Database belum dipilih">
        <p className="mb-2">Parameter <code>id</code> tidak ada di alamat halaman.</p>
        <BackLink />
      </Alert>
    )
  }
  return <DatabaseGate databaseId={databaseId}>{(database) => <Viewer database={database} />}</DatabaseGate>
}

mountPage(
  <Layout activePage="database" title="Database">
    <DatabaseRoute />
  </Layout>,
)
