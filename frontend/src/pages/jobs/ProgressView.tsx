import type { JobView } from '../../api/types'

export function ProgressView({ job }: { job: JobView }) {
  const p = job.progress
  if (p === null) {
    return <p className="text-secondary mb-0">Belum ada progres batch.</p>
  }
  return (
    <dl className="detail-list mb-0">
      <dt>Arah</dt>
      <dd>{p.direction === 'up' ? 'Up (menerapkan)' : 'Down (membatalkan)'}</dd>
      <dt>Batch committed</dt>
      <dd className="mono">{p.batches_committed}</dd>
      <dt>Baris committed</dt>
      <dd className="mono">{p.rows_committed}</dd>
      <dt>Batch reverted</dt>
      <dd className="mono">{p.batches_reverted}</dd>
      <dt>Baris reverted</dt>
      <dd className="mono">{p.rows_reverted}</dd>
      <dt>Total baris</dt>
      <dd className={p.total_rows === null ? 'text-secondary' : 'mono'}>{p.total_rows ?? 'Tidak diketahui'}</dd>
    </dl>
  )
}
