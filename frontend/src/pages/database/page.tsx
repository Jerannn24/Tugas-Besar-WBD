import { useCallback, useEffect, useRef, useState } from 'react'
import { Link, useParams, useSearchParams } from 'react-router'
import type { ApiFailure } from '../../api/client'
import { listSchemas } from '../../api/endpoints'
import type { DatabaseSummary, SchemaSummary } from '../../api/types'
import { AccessBadge } from '../../components/AccessBadge'
import { DatabaseGate } from '../../components/DatabaseGate'
import { EmptyState } from '../../components/EmptyState'
import { Page } from '../../components/Page'
import { Panel } from '../../components/Panel'
import { paths } from '../../lib/paths'
import { FailureAlert } from '../../components/FailureAlert'
import { SchemaSidebar, type Selection } from './SchemaSidebar'
import { TableView, type Tab } from './TableView'
import { ListSkeleton } from '../../components/Skeleton'

type SchemasState =
  | { status: 'loading' }
  | { status: 'ready'; schemas: SchemaSummary[] }
  | { status: 'failed'; failure: ApiFailure }

function BackLink() {
  return <Link to={paths.databases}>Kembali ke daftar database</Link>
}

function param(params: URLSearchParams, name: string): string | null {
  const value = params.get(name)
  return value === null || value.trim() === '' ? null : value
}

function Viewer({ database }: { database: DatabaseSummary }) {
  const [schemas, setSchemas] = useState<SchemasState>({ status: 'loading' })
  const [searchParams, setSearchParams] = useSearchParams()
  const schemaParam = param(searchParams, 'schema')
  const tableParam = param(searchParams, 'table')
  const selection: Selection | null =
    schemaParam !== null && tableParam !== null ? { schema: schemaParam, table: tableParam } : null
  const [tab, setTab] = useState<Tab>('columns')
  const schemasSeq = useRef(0)

  const fetchSchemas = useCallback(async () => {
    const seq = ++schemasSeq.current
    const res = await listSchemas(database.id)
    if (seq !== schemasSeq.current) return
    setSchemas(res.ok ? { status: 'ready', schemas: res.data.items } : { status: 'failed', failure: res })
  }, [database.id])

  const loadSchemas = useCallback(async () => {
    setSchemas({ status: 'loading' })
    await fetchSchemas()
  }, [fetchSchemas])

  useEffect(() => {
    void fetchSchemas()
    const ref = schemasSeq
    return () => {
      ref.current++
    }
  }, [fetchSchemas])

  function select(next: Selection) {
    setSearchParams({ schema: next.schema, table: next.table }, { replace: true })
  }

  return (
    <>
      <div className="d-flex flex-wrap align-items-center gap-2 mb-3">
        <h2 className="h2 mb-0 me-1 text-break">{database.name}</h2>
        <AccessBadge access={database.access} />
        <span className="ms-auto d-flex flex-wrap gap-3">
          <Link to={paths.migrations(database.id)}>Riwayat migration</Link>
          <BackLink />
        </span>
      </div>

      <div className="viewer-layout">
        <Panel title="Schema" className="mb-0">
          {schemas.status === 'loading' && <ListSkeleton label="Memuat schema…" items={6} heading />}
          {schemas.status === 'failed' && (
            <FailureAlert failure={schemas.failure} onRetry={() => void loadSchemas()} />
          )}
          {schemas.status === 'ready' && (
            <SchemaSidebar schemas={schemas.schemas} selected={selection} onSelect={select} />
          )}
        </Panel>

        <div className="viewer-main">
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

export function DatabasePage() {
  const { databaseId = '' } = useParams()
  return (
    <Page title="Database">
      <DatabaseGate key={databaseId} databaseId={databaseId}>
        {(database) => <Viewer database={database} />}
      </DatabaseGate>
    </Page>
  )
}
