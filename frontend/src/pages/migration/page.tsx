import type { ReactNode } from 'react'
import { Link, useParams } from 'react-router'
import type { DatabaseSummary } from '../../api/types'
import { AccessBadge } from '../../components/AccessBadge'
import { Alert } from '../../components/Alert'
import { DatabaseGate } from '../../components/DatabaseGate'
import { Page } from '../../components/Page'
import { paths } from '../../lib/paths'
import { CreateDraft } from './CreateDraft'
import { DraftEditor } from './DraftEditor'
import { ExecutionView } from './ExecutionView'

function Header({ database }: { database: DatabaseSummary }) {
  return (
    <div className="d-flex flex-wrap align-items-center gap-2 mb-3">
      <h2 className="h2 mb-0 me-1 text-break">{database.name}</h2>
      <AccessBadge access={database.access} />
      <span className="ms-auto d-flex flex-wrap align-items-center gap-3">
        <Link to={paths.proposals(database.id)}>Proposal</Link>
        <Link to={paths.migrations(database.id)}>Riwayat migration</Link>
      </span>
    </div>
  )
}

type FrameProps = {
  title: string
  requireEdit: boolean
  children: (database: DatabaseSummary) => ReactNode
}

function MigrationFrame({ title, requireEdit, children }: FrameProps) {
  const { databaseId = '' } = useParams()
  return (
    <Page title={title}>
      <DatabaseGate key={databaseId} databaseId={databaseId}>
        {(database) => (
          <>
            <Header database={database} />
            {requireEdit && database.access !== 'edit' ? (
              <Alert variant="danger" title="Akses ditolak">
                <p className="mb-2">Anda tidak memiliki akses untuk membuat atau mengubah migration pada database ini.</p>
                <Link to={paths.migrations(database.id)}>Lihat riwayat migration</Link>
              </Alert>
            ) : (
              children(database)
            )}
          </>
        )}
      </DatabaseGate>
    </Page>
  )
}

export function NewDraftPage() {
  return (
    <MigrationFrame title="Migration baru" requireEdit>
      {(database) => <CreateDraft database={database} />}
    </MigrationFrame>
  )
}

export function DraftPage() {
  const { draftId = '' } = useParams()
  return (
    <MigrationFrame title="Draft migration" requireEdit>
      {(database) => <DraftEditor key={draftId} database={database} draftId={draftId} />}
    </MigrationFrame>
  )
}

export function ExecutionPage() {
  const { executionId = '' } = useParams()
  return (
    <MigrationFrame title="Detail execution" requireEdit={false}>
      {(database) => <ExecutionView key={executionId} database={database} executionId={executionId} />}
    </MigrationFrame>
  )
}
