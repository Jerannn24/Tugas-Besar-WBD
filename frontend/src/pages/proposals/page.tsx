import type { ReactNode } from 'react'
import { Link, useParams } from 'react-router'
import type { DatabaseSummary } from '../../api/types'
import { AccessBadge } from '../../components/AccessBadge'
import { DatabaseGate } from '../../components/DatabaseGate'
import { Page } from '../../components/Page'
import { paths } from '../../lib/paths'
import { ProposalDetail } from './ProposalDetail'
import { ProposalList } from './ProposalList'

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
                <Link to={paths.proposals(database.id)}>Semua proposal</Link>
                <Link to={paths.jobs(database.id)}>Job</Link>
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

export function ProposalsPage() {
  return <Frame title="Proposal migration">{(database) => <ProposalList database={database} />}</Frame>
}

export function ProposalPage() {
  const { proposalId = '' } = useParams()
  return (
    <Frame title="Detail proposal">
      {(database) => <ProposalDetail key={proposalId} database={database} proposalId={proposalId} />}
    </Frame>
  )
}
