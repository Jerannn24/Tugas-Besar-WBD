import { Link } from 'react-router'
import type { JobStatus, JobView } from '../../api/types'
import { Alert } from '../../components/Alert'
import { JobBadge } from '../../components/StateBadge'
import { formatDateTime } from '../../lib/format'
import { paths } from '../../lib/paths'

type Variant = 'info' | 'success' | 'warning' | 'danger'

const UNRESOLVED: ReadonlySet<JobStatus> = new Set(['partial', 'partially_reverted', 'unknown'])

const outcomeOf = (job: JobView): JobStatus => job.action === 'resume' || UNRESOLVED.has(job.status) ? job.execution_status : job.status

const isRevert = (job: JobView) => job.action === 'rollback' || job.action === 'compensate'

function describe(job: JobView, outcome: JobStatus): { variant: Variant; title: string; text: string } {
  switch (outcome) {
    case 'queued':
      return { variant: 'info', title: 'Job diterima', text: 'Job sudah masuk antrean dan menunggu worker. SQL belum dijalankan.' }
    case 'running':
      return { variant: 'info', title: 'Sedang berjalan', text: 'Worker sedang menjalankan job ini.' }
    case 'retry_wait':
      return {
        variant: 'warning',
        title: 'Menunggu retry',
        text: `Percobaan sebelumnya gagal tanpa batch committed. Retry otomatis dijadwalkan ${formatDateTime(job.next_retry_at)}.`,
      }
    case 'succeeded':
      return {
        variant: 'success',
        title: isRevert(job) ? 'Pembatalan committed' : 'SQL committed',
        text:
          job.mode === 'batch'
            ? 'Seluruh batch selesai dan di-commit ke database target.'
            : 'SQL selesai dan di-commit ke database target.',
      }
    case 'failed':
      return job.error?.code === 'REVIEW_STALE'
        ? {
            variant: 'danger',
            title: 'SQL tidak dijalankan',
            text: 'Approval berubah sebelum eksekusi, jadi worker menghentikan job sebelum SQL. Proposal kembali menunggu review.',
          }
        : { variant: 'danger', title: 'Job gagal', text: 'Lihat error di bawah.' }
    case 'partial':
      return {
        variant: 'warning',
        title: 'Sebagian batch committed',
        text: 'Sebagian batch sudah di-commit, sisanya belum. Lanjutkan dengan resume atau batalkan dengan kompensasi.',
      }
    case 'partially_reverted':
      return {
        variant: 'warning',
        title: 'Sebagian batch sudah dibatalkan',
        text: 'Kompensasi baru membatalkan sebagian batch yang committed.',
      }
    case 'unknown':
      return {
        variant: 'warning',
        title: 'Hasil belum pasti',
        text: 'Worker tidak dapat memastikan apakah SQL sudah di-commit. Jangan jalankan ulang; sistem merekonsiliasi receipt sebelum melanjutkan.',
      }
  }
}

export function Outcome({ job }: { job: JobView }) {
  const outcome = outcomeOf(job)
  const d = describe(job, outcome)
  const scheduled = (job.status === 'partial' || job.status === 'partially_reverted') && job.next_retry_at !== null

  return (
    <Alert variant={d.variant} title={d.title}>
      <p className="mb-0">{d.text}</p>
      {scheduled && <p className="mb-0 mt-2">Retry otomatis dijadwalkan {formatDateTime(job.next_retry_at)}.</p>}
      {outcome !== job.status && (
        <p className="mb-0 mt-2">
          Status job ini <JobBadge status={job.status} />; hasil di atas memakai status execution terbaru
          {job.action === 'resume' ? '.' : ' (dilanjutkan oleh job lain, misalnya resume).'}
        </p>
      )}
      {job.error?.code === 'REVIEW_STALE' && (
        <p className="mb-0 mt-2">
          <Link to={paths.proposal(job.database_id, job.proposal_id)}>Tinjau ulang proposal</Link>
        </p>
      )}
      {outcome === 'succeeded' && (
        <p className="mb-0 mt-2">
          <Link to={paths.migrations(job.database_id)}>Lihat riwayat migration</Link>
        </p>
      )}
    </Alert>
  )
}
