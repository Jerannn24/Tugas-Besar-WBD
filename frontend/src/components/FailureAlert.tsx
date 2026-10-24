import { errorMessage, type ApiFailure } from '../api/client'
import { Alert } from './Alert'
import { Button } from './Button'

export type FailureKind = 'expired' | 'forbidden' | 'not_found' | 'unavailable' | 'error'

export function classifyFailure(failure: ApiFailure): FailureKind {
  if (failure.kind !== 'http') return 'error'
  if (failure.status === 401) return 'expired'
  if (failure.status === 403) return 'forbidden'
  if (failure.status === 404) return 'not_found'
  if (failure.status === 502 || failure.error.code === 'TARGET_UNAVAILABLE') return 'unavailable'
  return 'error'
}

type Props = {
  failure: ApiFailure
  notFoundTitle?: string
  forbiddenText?: string
  onRetry?: () => void
}

export function FailureAlert({
  failure,
  notFoundTitle = 'Data tidak ditemukan',
  forbiddenText = 'Anda tidak memiliki akses.',
  onRetry,
}: Props) {
  const kind = classifyFailure(failure)
  const retry = onRetry && (
    <Button variant="outline-secondary" className="mt-2" onClick={onRetry}>
      Coba lagi
    </Button>
  )

  switch (kind) {
    case 'expired':
      return (
        <Alert variant="warning" title="Sesi berakhir">
          Mengalihkan ke <a href="/login/">halaman login</a>…
        </Alert>
      )
    case 'forbidden':
      return (
        <Alert variant="danger" title="Akses ditolak">
          {forbiddenText}
        </Alert>
      )
    case 'not_found':
      return (
        <Alert variant="warning" title={notFoundTitle}>
          {errorMessage(failure)}
        </Alert>
      )
    case 'unavailable':
      return (
        <Alert variant="danger" title="Database target tidak dapat dihubungi">
          <p className="mb-0">Koneksi ke database target gagal. Coba lagi beberapa saat lagi.</p>
          {retry}
        </Alert>
      )
    case 'error':
      return (
        <Alert variant="danger" title="Gagal memuat data">
          <p className="mb-0">{errorMessage(failure)}</p>
          {retry}
        </Alert>
      )
  }
}
