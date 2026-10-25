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
    case 'denied':
      return <Denied status={state.failure.kind === 'http' ? state.failure.status : 0} />
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

const DENIED: Record<number, { title: string; text: string; variant: 'warning' | 'danger' }> = {
  400: {
    variant: 'warning',
    title: 'ID database tidak valid',
    text: 'Alamat halaman berisi ID database yang tidak valid. Periksa tautan yang Anda buka.',
  },
  403: {
    variant: 'danger',
    title: 'Akses ditolak',
    text: 'Anda tidak memiliki akses ke database ini. Minta admin platform menambahkan grant untuk group Anda (Platform Admin juga memerlukan grant).',
  },
  404: {
    variant: 'warning',
    title: 'Database tidak ditemukan',
    text: 'Database ini tidak ada atau sudah dihapus.',
  },
}

function Denied({ status }: { status: number }) {
  const d = DENIED[status] ?? DENIED[404]
  return (
    <Alert variant={d.variant} title={d.title}>
      <p className="mb-2">{d.text}</p>
      <a href="/databases/">Kembali ke daftar database</a>
    </Alert>
  )
}
