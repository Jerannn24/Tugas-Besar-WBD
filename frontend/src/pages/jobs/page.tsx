import { useCallback, useEffect, useRef, useState } from 'react'
import { Link, useLocation, useParams } from 'react-router'
import type { ApiFailure } from '../../api/client'
import { getJob } from '../../api/jobs'
import type { DatabaseSummary, ID, JobView } from '../../api/types'
import { AccessBadge } from '../../components/AccessBadge'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { DatabaseGate } from '../../components/DatabaseGate'
import { FailureAlert } from '../../components/FailureAlert'
import { Page } from '../../components/Page'
import { Panel } from '../../components/Panel'
import { ListSkeleton } from '../../components/Skeleton'
import { JobBadge } from '../../components/StateBadge'
import { formatDateTime } from '../../lib/format'
import { paths } from '../../lib/paths'
import type { JobRouteState } from '../proposals/enqueue'

type LoadState = { status: 'loading' } | { status: 'failed'; failure: ApiFailure } | { status: 'ready'; job: JobView }

function isRouteState(v: unknown): v is JobRouteState {
  return typeof v === 'object' && v !== null && typeof (v as JobRouteState).replayed === 'boolean'
}

function JobSummary({ database, jobId }: { database: DatabaseSummary; jobId: ID }) {
  const [state, setState] = useState<LoadState>({ status: 'loading' })
  const [refreshing, setRefreshing] = useState(false)
  const seq = useRef(0)
  const location = useLocation()
  const replayed = isRouteState(location.state) && location.state.replayed

  const request = useCallback(
    async () => {
      const mine = ++seq.current
      const res = await getJob(database.id, jobId)
      if (mine !== seq.current) return
      setRefreshing(false)
      setState(res.ok ? { status: 'ready', job: res.data } : { status: 'failed', failure: res })
    },
    [database.id, jobId],
  )

  const fetchJob = useCallback(
    (mode: 'initial' | 'refresh') => {
      if (mode === 'initial') setState({ status: 'loading' })
      else setRefreshing(true)
      return request()
    },
    [request],
  )

  useEffect(() => {
    void request()
    const ref = seq
    return () => {
      ref.current++
    }
  }, [request])

  if (state.status === 'loading') {
    return (
      <Panel title="Job">
        <ListSkeleton label="Memuat job…" items={5} />
      </Panel>
    )
  }
  if (state.status === 'failed') {
    return (
      <FailureAlert
        failure={state.failure}
        notFoundTitle="Job tidak ditemukan"
        invalidTitle="ID job tidak valid"
        onRetry={() => void fetchJob('initial')}
      />
    )
  }

  const job = state.job
  return (
    <Panel
      title="Job"
      actions={
        <Button variant="outline-secondary" pending={refreshing} onClick={() => void fetchJob('refresh')}>
          Muat ulang
        </Button>
      }
    >
      <Alert variant="info" title="Job diterima">
        <p className="mb-0">
          Permintaan execute sudah masuk antrean. Diterima belum berarti SQL selesai; worker menjalankannya secara
          asinkron.
        </p>
        {replayed && (
          <p className="mb-0 mt-2">Permintaan yang sama sudah pernah diterima sebelumnya; ini job yang sama.</p>
        )}
      </Alert>
      <dl className="detail-list">
        <dt>Status</dt>
        <dd>
          <JobBadge status={job.status} />
        </dd>
        <dt>Job ID</dt>
        <dd className="mono">{job.job_id}</dd>
        <dt>Execution ID</dt>
        <dd className="mono">{job.execution_id}</dd>
        <dt>Proposal</dt>
        <dd>
          <Link className="mono" to={paths.proposal(database.id, job.proposal_id)}>
            {job.proposal_id}
          </Link>
        </dd>
        <dt>Dibuat</dt>
        <dd>{formatDateTime(job.created_at)}</dd>
      </dl>
    </Panel>
  )
}

export function JobPage() {
  const { databaseId = '', jobId = '' } = useParams()
  return (
    <Page title="Job migration">
      <DatabaseGate key={databaseId} databaseId={databaseId}>
        {(database) => (
          <>
            <div className="d-flex flex-wrap align-items-center gap-2 mb-3">
              <h2 className="h2 mb-0 me-1 text-break">{database.name}</h2>
              <AccessBadge access={database.access} />
              <span className="ms-auto d-flex flex-wrap align-items-center gap-3">
                <Link to={paths.proposals(database.id)}>Proposal</Link>
                <Link to={paths.migrations(database.id)}>Riwayat migration</Link>
              </span>
            </div>
            <JobSummary key={jobId} database={database} jobId={jobId} />
          </>
        )}
      </DatabaseGate>
    </Page>
  )
}
