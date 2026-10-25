import { errorMessage, isCsrfError, type ApiFailure } from '../../api/client'
import type { DatabaseSummary, Draft, Execution } from '../../api/types'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { Loading } from '../../components/Loading'
import { pageUrl } from '../../lib/query'
import type { useExecute } from './execute'
import { ExecutionDetail } from './ExecutionDetail'

type Props = {
  database: DatabaseSummary
  draft: Draft
  exec: ReturnType<typeof useExecute>
  blockedReason: string | null
}

export function ExecutePanel({ database, draft, exec, blockedReason }: Props) {
  const { state } = exec
  const applied = draft.applied_execution_id !== null
  const historyUrl = pageUrl('/migrations/', { database: database.id })
  const detailUrl = (x: Execution) => pageUrl('/migration/', { database: database.id, execution: x.id })

  const inFlight = state.phase === 'posting' || state.phase === 'polling'
  const unresolved = state.phase === 'waiting' || state.phase === 'uncertain'
  const reason = applied
    ? 'Draft sudah applied.'
    : inFlight
      ? 'Execute sedang berjalan.'
      : unresolved
        ? 'Hasil execute sebelumnya belum diketahui.'
        : blockedReason

  return (
    <section className="card mb-3" aria-labelledby="execute-title">
      <div className="card-header">
        <h2 id="execute-title" className="card-title">
          Execute
        </h2>
      </div>
      <div className="card-body">
        {!applied && (
          <div className="d-flex flex-wrap align-items-center gap-3 mb-3">
            <Button
              variant="success"
              disabled={reason !== null}
              pending={inFlight}
              onClick={exec.open}
              aria-describedby={reason ? 'execute-reason' : undefined}
            >
              Execute revision {draft.revision}
            </Button>
            {reason && (
              <span id="execute-reason" className="text-secondary">
                {reason}
              </span>
            )}
          </div>
        )}

        <Result
          state={state}
          historyUrl={historyUrl}
          detailUrl={detailUrl}
          onRecheck={exec.recheck}
          onCheckStatus={() => void exec.checkStatus()}
        />
      </div>

      <ConfirmDialog
        open={state.phase === 'confirming'}
        title="Jalankan migration?"
        confirmLabel="Ya, jalankan"
        onCancel={exec.cancel}
        onConfirm={() => exec.confirm(draft.revision)}
      >
        <p>
          SQL berikut (revision tersimpan <strong>{draft.revision}</strong>) akan dijalankan pada database{' '}
          <strong>{database.name}</strong>. Seluruh statement di-commit bersama atau di-rollback jika gagal.
        </p>
        <pre className="sql-block">{draft.sql}</pre>
      </ConfirmDialog>
    </section>
  )
}

type ResultProps = {
  state: ReturnType<typeof useExecute>['state']
  historyUrl: string
  detailUrl: (x: Execution) => string
  onRecheck: () => void
  onCheckStatus: () => void
}

function Result({ state, historyUrl, detailUrl, onRecheck, onCheckStatus }: ResultProps) {
  switch (state.phase) {
    case 'idle':
    case 'confirming':
      return null
    case 'posting':
      return <Loading label="Mengirim permintaan execute…" />
    case 'polling':
      return (
        <>
          <Loading label="Migration diterima, menunggu hasil (cek otomatis tiap 2 detik)…" />
          <ExecutionDetail execution={state.execution} showSql={false} />
        </>
      )
    case 'waiting':
      return (
        <>
          <Alert variant="warning" title="Hasil belum final">
            Status terakhir masih <strong>{state.execution.status}</strong>. Pengecekan otomatis dihentikan; jangan
            jalankan ulang. Gunakan tombol di bawah untuk mengecek status lagi.
          </Alert>
          {state.checkFailure && <CheckFailure failure={state.checkFailure} />}
          <ExecutionDetail execution={state.execution} showSql={false} />
          <div className="d-flex flex-wrap gap-3 align-items-center">
            <Button variant="outline-secondary" pending={state.checking} onClick={onCheckStatus}>
              Cek status
            </Button>
            <a href={detailUrl(state.execution)}>Lihat detail execution</a>
          </div>
        </>
      )
    case 'final':
      return (
        <>
          {state.execution.status === 'succeeded' ? (
            <Alert variant="success" title="Migration berhasil">
              SQL berhasil dijalankan dan di-commit. Draft ini sekarang applied.
            </Alert>
          ) : (
            <Alert variant="danger" title="Migration gagal">
              Request diterima, tetapi SQL gagal dan di-rollback. Perbaiki draft lalu jalankan lagi bila perlu.
            </Alert>
          )}
          <ExecutionDetail execution={state.execution} showSql={false} />
          <a href={detailUrl(state.execution)}>Lihat detail execution</a>
        </>
      )
    case 'uncertain':
      return (
        <Alert variant="warning" title="Hasil belum diketahui">
          <p className="mb-2">
            Koneksi terputus atau waktu habis sebelum respons diterima ({errorMessage(state.failure)}). Migration
            mungkin sudah dijalankan. Tombol di bawah mengirim ulang <em>permintaan yang sama</em> (request ID dan
            revision sama) sehingga server tidak menjalankannya dua kali.
          </p>
          <Button variant="outline-secondary" onClick={onRecheck}>
            Cek ulang
          </Button>
        </Alert>
      )
    case 'rejected':
      return <Rejected failure={state.failure} historyUrl={historyUrl} />
  }
}

function CheckFailure({ failure }: { failure: ApiFailure }) {
  return (
    <Alert variant="danger" title="Gagal mengecek status">
      {errorMessage(failure)}
    </Alert>
  )
}

function Rejected({ failure, historyUrl }: { failure: ApiFailure; historyUrl: string }) {
  const code = isCsrfError(failure) ? 'CSRF_INVALID' : failure.kind === 'http' ? failure.error.code : ''
  const message = errorMessage(failure)

  switch (code) {
    case 'DATABASE_BUSY':
      return (
        <Alert variant="warning" title="Database sedang sibuk">
          {message} Database target sedang menjalankan migration lain; coba lagi nanti.
        </Alert>
      )
    case 'SQL_UNSUPPORTED':
      return (
        <Alert variant="danger" title="SQL ditolak (tidak dijalankan)">
          <p className="mb-0" style={{ whiteSpace: 'pre-wrap' }}>
            {message}
          </p>
        </Alert>
      )
    case 'REQUEST_CONFLICT':
    case 'ALREADY_APPLIED':
      return (
        <Alert variant="warning" title={code === 'ALREADY_APPLIED' ? 'Draft sudah applied' : 'Permintaan bentrok'}>
          <p className="mb-2">{message}</p>
          <a href={historyUrl}>Lihat riwayat migration</a>
        </Alert>
      )
    case 'VERSION_CONFLICT':
      return (
        <Alert variant="warning" title="Draft berubah di tempat lain">
          {message} Muat versi terbaru draft sebelum menjalankan.
        </Alert>
      )
    case 'TARGET_UNAVAILABLE':
      return (
        <Alert variant="danger" title="Execute ditolak">
          SQL tidak dijalankan: database target belum siap untuk eksekusi atau tidak dapat dihubungi.
        </Alert>
      )
    case 'FORBIDDEN':
      return (
        <Alert variant="danger" title="Akses ditolak">
          Anda tidak memiliki akses.
        </Alert>
      )
    case 'CSRF_INVALID':
      return (
        <Alert variant="warning" title="Sesi keamanan diperbarui">
          Sesi keamanan diperbarui, silakan kirim ulang. SQL belum dijalankan.
        </Alert>
      )
    default:
      return (
        <Alert variant="danger" title="Execute ditolak">
          {message}
        </Alert>
      )
  }
}
