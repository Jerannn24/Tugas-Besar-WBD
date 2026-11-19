import type { ReactNode } from 'react'
import { Link, useParams } from 'react-router'
import type { DatabaseSummary } from '../../api/types'
import { AccessBadge } from '../../components/AccessBadge'
import { DatabaseGate } from '../../components/DatabaseGate'
import { Page } from '../../components/Page'
import { paths } from '../../lib/paths'
import { AttemptList } from './AttemptList'
import { JobDetail } from './JobDetail'
import { JobList } from './JobList'

function Frame({ title, children }: { title: string; children: (database: DatabaseSummary) => ReactNode }) {
  const { databaseId = '' } = useParams()
  return (
    <Page title={title}>
      <DatabaseGate key={databaseId} databaseId={databaseId}>
        {(database) => (
          <>
            <div className="d-flex flex-wrap align-items-center gap-2 mb-3">
              <h2 className="h2 mb-0 me-1 text-break">{database.name}</h2>
              <AccessBadge access={database.access} />
              <span className="ms-auto d-flex flex-wrap align-items-center gap-3">
                <Link to={paths.proposals(database.id)}>Proposal</Link>
                <Link to={paths.jobs(database.id)}>Job</Link>
                <Link to={paths.attempts(database.id)}>Attempt</Link>
                <Link to={paths.migrations(database.id)}>Riwayat migration</Link>
              </span>
            </div>
            {children(database)}
          </>
        )}
      </DatabaseGate>
    </Page>
  )
}

export function JobsPage() {
  return <Frame title="Job migration">{(database) => <JobList database={database} />}</Frame>
}

export function JobPage() {
  const { jobId = '' } = useParams()
  return (
    <Frame title="Detail job">{(database) => <JobDetail key={jobId} database={database} jobId={jobId} />}</Frame>
  )
}

export function AttemptsPage() {
  return <Frame title="Attempt eksekusi">{(database) => <AttemptList database={database} />}</Frame>
}
