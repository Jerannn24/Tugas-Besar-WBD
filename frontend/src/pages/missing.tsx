import { Link } from 'react-router'
import { Alert } from '../components/Alert'
import { Page } from '../components/Page'
import { paths } from '../lib/paths'

export function MissingPage() {
  return (
    <Page title="Halaman tidak ditemukan">
      <Alert variant="warning" title="Halaman tidak ditemukan">
        <p className="mb-2">Alamat yang Anda buka tidak tersedia.</p>
        <Link to={paths.databases}>Kembali ke daftar database</Link>
      </Alert>
    </Page>
  )
}
