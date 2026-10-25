import type { Execution } from '../../api/types'
import { Alert } from '../../components/Alert'
import { StatusBadge } from '../../components/StatusBadge'
import { formatDateTime } from '../../lib/format'

const ERROR_LABELS: Record<string, string> = {
  SQL_FAILED: 'SQL gagal dijalankan; perubahan di-rollback',
  RECEIPT_FAILED: 'Pencatatan hasil di database target gagal; perubahan di-rollback',
  TARGET_PREPARATION_FAILED: 'Persiapan database target gagal',
  ACCESS_REVOKED: 'Akses dicabut sebelum migration dijalankan',
  COMMIT_UNCONFIRMED: 'Commit belum terkonfirmasi',
  RECEIPT_UNCONFIRMED: 'Catatan hasil di database target belum terkonfirmasi',
  RECEIPT_MISMATCH: 'Catatan hasil di database target tidak cocok',
  ROLLBACK_UNCONFIRMED: 'Rollback belum terkonfirmasi',
  OUTCOME_UNCONFIRMED: 'Hasil akhir belum terkonfirmasi',
}

type Props = { execution: Execution; showSql?: boolean }

export function ExecutionDetail({ execution: x, showSql = true }: Props) {
  return (
    <>
      <dl className="detail-list">
        <dt>Version</dt>
        <dd className="mono">v{x.version}</dd>
        <dt>Title</dt>
        <dd>{x.title}</dd>
        <dt>Status</dt>
        <dd>
          <StatusBadge status={x.status} />
        </dd>
        <dt>Dijalankan oleh</dt>
        <dd>{x.executed_by.username}</dd>
        <dt>Mulai</dt>
        <dd>{formatDateTime(x.started_at)}</dd>
        <dt>Selesai</dt>
        <dd>{formatDateTime(x.finished_at)}</dd>
        <dt>Revision draft</dt>
        <dd>{x.draft_revision}</dd>
      </dl>

      {x.status === 'running' && (
        <Alert variant="info" title="Masih berjalan">
          Migration sedang dijalankan. Hasil akhir belum tersedia.
        </Alert>
      )}
      {x.status === 'unknown' && (
        <Alert variant="warning" title="Hasil belum diketahui">
          Hasil execution ini belum terbukti berhasil maupun gagal. Jangan jalankan ulang; tunggu sampai status
          terselesaikan.
        </Alert>
      )}
      {x.error && (
        <Alert variant="danger" title="Error">
          <p className="mb-1">
            {ERROR_LABELS[x.error.code] && <strong className="me-2">{ERROR_LABELS[x.error.code]}</strong>}
            <span className="mono">{x.error.code}</span>
          </p>
          <p className="mb-0" style={{ whiteSpace: 'pre-wrap' }}>
            {x.error.message}
          </p>
        </Alert>
      )}

      {showSql && (
        <>
          <h3 className="h4">SQL yang dijalankan (snapshot)</h3>
          <pre className="sql-block">{x.sql}</pre>
        </>
      )}
    </>
  )
}
