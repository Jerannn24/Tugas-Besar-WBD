import type { Snapshot } from '../api/types'

export type Spec = Pick<Snapshot, 'title' | 'revision' | 'sql_up' | 'sql_down' | 'irreversible_reason' | 'mode' | 'batch'>

export function MigrationSpec({ spec }: { spec: Spec }) {
  return (
    <>
      <dl className="detail-list">
        <dt>Title</dt>
        <dd>{spec.title}</dd>
        <dt>Revision</dt>
        <dd>{spec.revision}</dd>
        <dt>Mode</dt>
        <dd>{spec.mode === 'batch' ? 'Batch' : 'Standard'}</dd>
      </dl>

      <h3 className="h4">SQL up</h3>
      <pre className="sql-block mb-3">{spec.sql_up}</pre>

      {spec.sql_down !== null ? (
        <>
          <h3 className="h4">SQL down</h3>
          <pre className="sql-block mb-3">{spec.sql_down}</pre>
        </>
      ) : (
        <>
          <h3 className="h4">Tidak dapat dibatalkan</h3>
          <p className="text-break">{spec.irreversible_reason ?? '—'}</p>
        </>
      )}

      {spec.batch && (
        <>
          <h3 className="h4">Konfigurasi batch</h3>
          <dl className="detail-list">
            <dt>Schema</dt>
            <dd>{spec.batch.schema}</dd>
            <dt>Table</dt>
            <dd>{spec.batch.table}</dd>
            <dt>Kolom cursor</dt>
            <dd>{spec.batch.cursor_column}</dd>
            <dt>Ukuran batch</dt>
            <dd>{spec.batch.batch_size}</dd>
          </dl>
          <h4 className="h5">Parameter</h4>
          <pre className="sql-block mb-3">{JSON.stringify(spec.batch.parameters, null, 2)}</pre>
        </>
      )}
    </>
  )
}
