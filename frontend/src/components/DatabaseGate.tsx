import type { ReactNode } from 'react'
import type { DatabaseSummary, ID } from '../api/types'
import { useDatabaseAccess } from '../lib/access'
import { Alert } from './Alert'
import { FailureAlert } from './FailureAlert'
import { Loading } from './Loading'

type Props = { databaseId: ID; children: (database: DatabaseSummary) => ReactNode }

export function DatabaseGate({ databaseId, children }: Props) {
  const { state, reload } = useDatabaseAccess(databaseId)

  switch (state.status) {
    case 'loading':
      return <Loading label="Memuat informasi database…" />
    case 'failed':
      return <FailureAlert failure={state.failure} onRetry={() => void reload()} />
    case 'missing':
      return (
        <Alert variant="warning" title="Database tidak tersedia">
          <p className="mb-2">Database tidak ditemukan atau Anda tidak memiliki akses ke database ini.</p>
          <a href="/databases/">Kembali ke daftar database</a>
        </Alert>
      )
    case 'ready':
      return <>{children(state.database)}</>
  }
}
