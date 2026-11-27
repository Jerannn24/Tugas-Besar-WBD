import { isRouteErrorResponse, useRouteError } from 'react-router'
import { Alert } from '../components/Alert'
import { paths } from '../lib/paths'

export function RouteErrorPage() {
  const error = useRouteError()
  const notFound = isRouteErrorResponse(error) && error.status === 404
  return (
    <main className="page page-center">
      <div className="container container-tight py-4">
        <Alert variant="danger" title={notFound ? 'Halaman tidak ditemukan' : 'Terjadi kesalahan pada aplikasi'}>
          <p className="mb-2">Muat ulang halaman atau kembali ke daftar database.</p>
          <a href={paths.databases}>Kembali ke daftar database</a>
        </Alert>
      </div>
    </main>
  )
}
