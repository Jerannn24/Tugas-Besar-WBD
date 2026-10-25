import { useCallback, useEffect, useRef, useState } from 'react'
import type { ApiFailure } from '../../api/client'
import { getRows, getTable } from '../../api/endpoints'
import type { Column, Constraint, ID, Index, TableMetadata, TableRows } from '../../api/types'
import { Button } from '../../components/Button'
import { DataTable, type DataColumn } from '../../components/DataTable'
import { EmptyState } from '../../components/EmptyState'
import { Loading } from '../../components/Loading'
import { NullValue } from '../../components/NullValue'
import { FailureAlert } from '../../components/FailureAlert'
import type { Selection } from './SchemaSidebar'

export const PAGE_SIZE = 50

export type Tab = 'columns' | 'constraints' | 'indexes' | 'data'

type Load<T> =
  | { status: 'loading' }
  | { status: 'ready'; data: T }
  | { status: 'failed'; failure: ApiFailure }

type Row = (string | null)[]

const TABS: { id: Tab; label: string }[] = [
  { id: 'columns', label: 'Columns' },
  { id: 'constraints', label: 'Constraints' },
  { id: 'indexes', label: 'Indexes' },
  { id: 'data', label: 'Data' },
]

const COLUMN_COLUMNS: DataColumn<Column>[] = [
  { key: 'name', header: 'Nama', render: (c) => <strong>{c.name}</strong> },
  { key: 'type', header: 'Tipe', render: (c) => <span className="mono">{c.data_type}</span> },
  { key: 'nullable', header: 'Nullable', render: (c) => (c.nullable ? 'Ya' : 'Tidak') },
  {
    key: 'default',
    header: 'Default',
    wrap: true,
    render: (c) =>
      c.default_expression === null ? (
        <span className="text-secondary">—</span>
      ) : (
        <span className="mono">{c.default_expression}</span>
      ),
  },
]

const CONSTRAINT_KIND_LABELS: Record<string, string> = {
  primary: 'PRIMARY KEY',
  foreign: 'FOREIGN KEY',
  unique: 'UNIQUE',
  check: 'CHECK',
  exclusion: 'EXCLUDE',
  other: 'Lainnya',
}

function constraintKindLabel(kind: string): string {
  return Object.hasOwn(CONSTRAINT_KIND_LABELS, kind) ? CONSTRAINT_KIND_LABELS[kind] : kind
}

const CONSTRAINT_COLUMNS: DataColumn<Constraint>[] = [
  { key: 'name', header: 'Nama', render: (c) => <strong>{c.name}</strong> },
  { key: 'kind', header: 'Jenis', render: (c) => constraintKindLabel(c.kind) },
  { key: 'definition', header: 'Definisi', wrap: true, render: (c) => <span className="mono">{c.definition}</span> },
]

const INDEX_COLUMNS: DataColumn<Index>[] = [
  { key: 'name', header: 'Nama', render: (i) => <strong>{i.name}</strong> },
  { key: 'definition', header: 'Definisi', wrap: true, render: (i) => <span className="mono">{i.definition}</span> },
  { key: 'unique', header: 'Unique', render: (i) => (i.is_unique ? 'Ya' : 'Tidak') },
  {
    key: 'primary',
    header: 'Primary',
    render: (i) => (i.is_primary ? <span className="badge bg-blue-lt">Primary</span> : 'Tidak'),
  },
]

type Props = {
  databaseId: ID
  selection: Selection
  tab: Tab
  onTabChange: (tab: Tab) => void
}

export function TableView({ databaseId, selection, tab, onTabChange }: Props) {
  const { schema, table } = selection
  const [meta, setMeta] = useState<Load<TableMetadata>>({ status: 'loading' })
  const [rows, setRows] = useState<Load<TableRows>>({ status: 'loading' })
  const [rowsOffset, setRowsOffset] = useState(0)

  const metaSeq = useRef(0)
  const rowsSeq = useRef(0)

  const fetchMeta = useCallback(async () => {
    const seq = ++metaSeq.current
    const res = await getTable(databaseId, schema, table)
    if (seq !== metaSeq.current) return
    setMeta(res.ok ? { status: 'ready', data: res.data } : { status: 'failed', failure: res })
  }, [databaseId, schema, table])

  const fetchRows = useCallback(
    async (offset: number) => {
      const seq = ++rowsSeq.current
      const res = await getRows(databaseId, { schema, table, limit: PAGE_SIZE, offset })
      if (seq !== rowsSeq.current) return
      setRows(res.ok ? { status: 'ready', data: res.data } : { status: 'failed', failure: res })
    },
    [databaseId, schema, table],
  )

  const loadRows = useCallback(
    (offset: number) => {
      setRowsOffset(offset)
      setRows({ status: 'loading' })
      return fetchRows(offset)
    },
    [fetchRows],
  )

  const reloadAll = useCallback(() => {
    setMeta({ status: 'loading' })
    void fetchMeta()
    void loadRows(0)
  }, [fetchMeta, loadRows])

  useEffect(() => {
    void fetchMeta()
    void fetchRows(0)
    const metaRef = metaSeq
    const rowsRef = rowsSeq
    return () => {
      metaRef.current++
      rowsRef.current++
    }
  }, [fetchMeta, fetchRows])

  return (
    <section className="card" aria-labelledby="table-view-title">
      <div className="card-header flex-column align-items-start gap-2">
        <h2 id="table-view-title" className="card-title text-break">
          <span className="text-secondary">{schema}</span>
          <span className="text-secondary mx-1">.</span>
          {table}
        </h2>
        <div className="nav nav-tabs card-header-tabs" role="tablist">
          {TABS.map((t) => (
            <button
              key={t.id}
              type="button"
              role="tab"
              id={`tab-${t.id}`}
              aria-selected={tab === t.id}
              aria-controls="table-view-panel"
              className={`nav-link${tab === t.id ? ' active' : ''}`}
              onClick={() => onTabChange(t.id)}
            >
              {t.label}
              {meta.status === 'ready' && t.id !== 'data' && (
                <span className="badge bg-secondary-lt ms-2">{metaCount(meta.data, t.id)}</span>
              )}
            </button>
          ))}
        </div>
      </div>
      <div className="card-body" role="tabpanel" id="table-view-panel" aria-labelledby={`tab-${tab}`}>
        {tab === 'data' ? (
          <RowsPanel
            rows={rows}
            offset={rowsOffset}
            onPage={(offset) => void loadRows(offset)}
            onRetry={() => void loadRows(rowsOffset)}
          />
        ) : (
          <MetaPanel meta={meta} tab={tab} onRetry={reloadAll} />
        )}
      </div>
    </section>
  )
}

function metaCount(meta: TableMetadata, tab: Exclude<Tab, 'data'>): number {
  if (tab === 'columns') return meta.columns.length
  if (tab === 'constraints') return meta.constraints.length
  return meta.indexes.length
}

function MetaPanel({
  meta,
  tab,
  onRetry,
}: {
  meta: Load<TableMetadata>
  tab: Exclude<Tab, 'data'>
  onRetry: () => void
}) {
  if (meta.status === 'loading') return <Loading label="Memuat metadata table…" />
  if (meta.status === 'failed') {
    return <FailureAlert failure={meta.failure} notFoundTitle="Table tidak ditemukan" onRetry={onRetry} />
  }

  const { columns, constraints, indexes } = meta.data
  switch (tab) {
    case 'columns':
      return columns.length === 0 ? (
        <EmptyState title="Table tidak memiliki column" />
      ) : (
        <DataTable columns={COLUMN_COLUMNS} rows={columns} rowKey={(c) => c.name} caption="Daftar column" />
      )
    case 'constraints':
      return constraints.length === 0 ? (
        <EmptyState title="Tidak ada constraint" />
      ) : (
        <DataTable
          columns={CONSTRAINT_COLUMNS}
          rows={constraints}
          rowKey={(c) => c.name}
          caption="Daftar constraint"
        />
      )
    case 'indexes':
      return indexes.length === 0 ? (
        <EmptyState title="Tidak ada index" />
      ) : (
        <DataTable columns={INDEX_COLUMNS} rows={indexes} rowKey={(i) => i.name} caption="Daftar index" />
      )
  }
}

function RowsPanel({
  rows,
  offset,
  onPage,
  onRetry,
}: {
  rows: Load<TableRows>
  offset: number
  onPage: (offset: number) => void
  onRetry: () => void
}) {
  if (rows.status === 'loading') return <Loading label="Memuat data…" />
  if (rows.status === 'failed') {
    return <FailureAlert failure={rows.failure} notFoundTitle="Table tidak ditemukan" onRetry={onRetry} />
  }

  const data = rows.data
  const prevOffset = Math.max(0, offset - PAGE_SIZE)

  if (data.rows.length === 0 && offset === 0) {
    return <EmptyState title="Table kosong (0 baris)">Table ini ada, tetapi belum berisi data.</EmptyState>
  }

  const columns: DataColumn<Row>[] = data.columns.map((c, j) => ({
    key: String(j),
    header: (
      <>
        <span className="text-reset">{c.name}</span>
        <div className="text-secondary small fw-normal mono">{c.data_type}</div>
      </>
    ),
    render: (row) => <Cell value={row[j]} />,
  }))

  return (
    <>
      {data.rows.length === 0 ? (
        <EmptyState title="Tidak ada baris di halaman ini" />
      ) : (
        <DataTable
          columns={columns}
          rows={data.rows}
          rowKey={(_, i) => String(offset + i)}
          caption="Data table"
        />
      )}
      <div className="d-flex flex-wrap align-items-center justify-content-between gap-2 mt-3">
        <span className="text-secondary">
          {data.rows.length > 0
            ? `Baris ${offset + 1}–${offset + data.rows.length}`
            : `Offset ${offset}`}
        </span>
        <div className="d-flex gap-2">
          <Button variant="outline-secondary" disabled={offset === 0} onClick={() => onPage(prevOffset)}>
            Sebelumnya
          </Button>
          <Button
            variant="outline-secondary"
            disabled={!data.has_more}
            onClick={() => onPage(offset + data.rows.length)}
          >
            Berikutnya
          </Button>
        </div>
      </div>
    </>
  )
}

function Cell({ value }: { value: string | null | undefined }) {
  if (value === null) return <NullValue />
  return <span className="cell-value">{value ?? ''}</span>
}
