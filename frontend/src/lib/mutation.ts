import { errorMessage, type ApiFailure } from '../api/client'

export type MutationFailure = { title: string; text: string; fieldErrors: Record<string, string> }

export function describeFailure(failure: ApiFailure, fallbackTitle: string): MutationFailure | null {
  if (failure.kind !== 'http') return { title: fallbackTitle, text: errorMessage(failure), fieldErrors: {} }
  if (failure.status === 401) return null

  const { code, field_errors } = failure.error
  const text = errorMessage(failure)
  switch (code) {
    case 'FORBIDDEN':
      return { title: 'Akses ditolak', text, fieldErrors: {} }
    case 'CSRF_INVALID':
      return { title: 'Sesi keamanan diperbarui', text, fieldErrors: {} }
    case 'NOT_FOUND':
      return { title: 'Data tidak ditemukan', text: `${text} Data mungkin sudah dihapus di tempat lain; muat ulang halaman.`, fieldErrors: {} }
    case 'DATABASE_BUSY':
      return { title: 'Database target sedang sibuk', text, fieldErrors: {} }
    case 'TARGET_UNAVAILABLE':
      return { title: 'Database target tidak dapat dihubungi', text, fieldErrors: {} }
    case 'VALIDATION_FAILED':
      return { title: fallbackTitle, text, fieldErrors: field_errors }
    default:
      return { title: fallbackTitle, text, fieldErrors: {} }
  }
}
