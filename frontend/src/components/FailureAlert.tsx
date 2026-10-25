import { errorMessage, type ApiFailure } from '../api/client'
import { Alert } from './Alert'
import { Button } from './Button'
import { classifyFailure } from '../lib/failure'

type Props = {
  failure: ApiFailure
  notFoundTitle?: string
  invalidTitle?: string
  forbiddenText?: string
  onRetry?: () => void
}

export function FailureAlert({
  failure,
  notFoundTitle = 'Data tidak ditemukan',
  invalidTitle = 'Permintaan tidak valid',
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
    case 'invalid':
      return (
        <Alert variant="warning" title={invalidTitle}>
          {errorMessage(failure)} Periksa alamat halaman.
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
