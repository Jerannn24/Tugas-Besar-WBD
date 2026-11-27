import { Link, useLocation } from 'react-router'
import { errorMessage } from '../../api/client'
import type { DatabaseSummary, ID, JobError, JobView } from '../../api/types'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { FailureAlert } from '../../components/FailureAlert'
import { Panel } from '../../components/Panel'
import { ShortId } from '../../components/ShortId'
import { ListSkeleton } from '../../components/Skeleton'
import { JobBadge } from '../../components/StateBadge'
import { formatDateTime } from '../../lib/format'
import { paths } from '../../lib/paths'
import { ACTION_LABELS } from '../proposals/labels'
import type { JobRouteState } from '../proposals/enqueue'
import { JobActions } from './JobActions'
import { Outcome } from './Outcome'
import { POLL_MS, useJob } from './poll'
import { ProgressView } from './ProgressView'

function isRouteState(v: unknown): v is JobRouteState {
  return typeof v === 'object' && v !== null && typeof (v as JobRouteState).replayed === 'boolean'
}

type Props = { database: DatabaseSummary; jobId: ID }

export function JobDetail({ database, jobId }: Props) {
  const { state, refresh, polling } = useJob(database.id, jobId)
  const location = useLocation()
  const replayed = isRouteState(location.state) && location.state.replayed

  if (state.status === 'loading') {
    return (
      <Panel title="Job">
        <ListSkeleton label="Memuat job…" items={6} heading />
      </Panel>
    )
  }
  if (state.status === 'failed') {
    return (
      <FailureAlert
        failure={state.failure}
        notFoundTitle="Job tidak ditemukan"
        invalidTitle="ID job tidak valid"
        onRetry={() => void refresh()}
      />
    )
  }

  const job = state.job
  return (
    <>
      <Panel
        title={
          <span className="d-inline-flex flex-wrap align-items-center gap-2">
            Job <ShortId id={job.job_id} /> <JobBadge status={job.status} />
          </span>
        }
        actions={
          <Button variant="outline-secondary" pending={state.refreshing} onClick={() => void refresh()}>
            Muat ulang
          </Button>
        }
      >
        <p className="text-secondary small" aria-live="polite">
          {polling
            ? `Diperbarui otomatis tiap ${POLL_MS / 1000} detik.`
            : 'Pemantauan otomatis berhenti.'}{' '}
          Terakhir diperbarui {formatDateTime(new Date(state.updatedAt).toISOString())}.
        </p>
        {replayed && (
          <Alert variant="info" title="Permintaan sama">
            Permintaan yang sama sudah pernah diterima sebelumnya; ini job yang sama.
          </Alert>
        )}
        {state.pollFailure && (
          <Alert variant="warning" title="Gagal memperbarui status">
            {errorMessage(state.pollFailure)} Data di bawah mungkin bukan yang terbaru.
          </Alert>
        )}
        <Outcome job={job} />
        {job.blocks_database && (
          <Alert variant="warning" title="Database tertahan">
            Migration lain pada database ini tidak dapat berjalan sampai job ini diselesaikan (resume, kompensasi, atau
            rekonsiliasi).
          </Alert>
        )}
        {job.error && <JobErrorAlert job={job} error={job.error} />}
        <JobFacts database={database} job={job} />
      </Panel>

      {job.mode === 'batch' && (
        <Panel title="Progres batch">
          <ProgressView job={job} />
        </Panel>
      )}
      <JobActions job={job} onReload={() => void refresh()} />
    </>
  )
}

function JobErrorAlert({ job, error }: { job: JobView; error: JobError }) {
  return (
    <Alert variant="danger" title="Error">
      <p className="mb-0 text-break">
        <code>{error.code}</code>: {error.message}
      </p>
      {error.retryable && job.next_retry_at !== null && (
        <p className="mb-0 mt-2">Akan dicoba ulang otomatis {formatDateTime(job.next_retry_at)}.</p>
      )}
    </Alert>
  )
}

function JobFacts({ database, job }: { database: DatabaseSummary; job: JobView }) {
  return (
    <dl className="detail-list mb-0">
      <dt>Job ID</dt>
      <dd className="mono">{job.job_id}</dd>
      <dt>Execution ID</dt>
      <dd className="mono">{job.execution_id}</dd>
      <dt>Proposal</dt>
      <dd>
        <Link to={paths.proposal(database.id, job.proposal_id)}>
          <ShortId id={job.proposal_id} />
        </Link>
      </dd>
      <dt>Migration</dt>
      <dd>
        {database.access === 'edit' ? (
          <Link to={paths.draft(database.id, job.migration_id)}>
            <ShortId id={job.migration_id} />
          </Link>
        ) : (
          <ShortId id={job.migration_id} />
        )}{' '}
        revision {job.revision}
      </dd>
      <dt>Aksi</dt>
      <dd>
        {ACTION_LABELS[job.action] ?? job.action} · {job.mode === 'batch' ? 'Batch' : 'Standard'}
      </dd>
      <dt>Hasil execution</dt>
      <dd>
        <JobBadge status={job.execution_status} />
      </dd>
      <dt>Attempt</dt>
      <dd>{job.attempt_no}</dd>
      <dt>Source execution</dt>
      <dd>{job.source_execution_id ? <ShortId id={job.source_execution_id} /> : '—'}</dd>
      <dt>Dibuat</dt>
      <dd>{formatDateTime(job.created_at)}</dd>
      <dt>Mulai</dt>
      <dd>{formatDateTime(job.started_at)}</dd>
      <dt>Selesai</dt>
      <dd>{formatDateTime(job.finished_at)}</dd>
      <dt>Retry berikutnya</dt>
      <dd>{formatDateTime(job.next_retry_at)}</dd>
    </dl>
  )
}
