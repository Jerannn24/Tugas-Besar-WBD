import { errorMessage, type ApiFailure } from '../api/client'
import { classifyWorkflowFailure, type WorkflowFailureKind } from '../lib/failure'
import { Alert } from './Alert'

const TITLES: Record<WorkflowFailureKind, string> = {
  network: 'Tidak dapat menghubungi server',
  invalid_response: 'Respons server tidak dikenali',
  invalid_request: 'Permintaan tidak valid',
  session_ended: 'Sesi berakhir',
  csrf: 'Sesi keamanan diperbarui',
  forbidden: 'Akses ditolak',
  not_found: 'Data tidak ditemukan',
  conflict: 'Data berubah',
  validation: 'Ditolak oleh validasi',
  not_implemented: 'Fitur belum tersedia',
  target_unavailable: 'Database target tidak dapat dihubungi',
  queue_unavailable: 'Antrean tidak tersedia',
  server: 'Terjadi kesalahan pada server',
}

const CODE_TITLES: Record<string, string> = {
  LEADER_REQUIRED: 'Bukan leader group',
  REQUEST_CONFLICT: 'Permintaan bentrok',
  APPROVAL_REQUIRED: 'Approval belum lengkap',
  REVIEW_STALE: 'Review sudah usang',
  VERSION_CONFLICT: 'Revisi sudah berubah',
  DATABASE_BUSY: 'Database sedang sibuk',
}

const OWN_TITLE: ReadonlySet<WorkflowFailureKind> = new Set(['invalid_request', 'validation', 'server'])

type Props = {
  failure: ApiFailure | null
  title?: string
  shownFields?: string[]
  note?: string
}

export function WorkflowAlert({ failure, title, shownFields = [], note }: Props) {
  if (!failure) return null
  const w = classifyWorkflowFailure(failure)
  if (w.kind === 'session_ended') return null

  const variant = w.kind === 'conflict' || w.kind === 'csrf' || w.kind === 'queue_unavailable' ? 'warning' : 'danger'
  const heading = (w.code && CODE_TITLES[w.code]) ?? (title && OWN_TITLE.has(w.kind) ? title : TITLES[w.kind])
  const others = Object.entries(w.fieldErrors).filter(([field]) => !shownFields.includes(field))

  return (
    <Alert variant={variant} title={heading}>
      <p className="mb-0">{errorMessage(failure)}</p>
      {note && <p className="mb-0 mt-2">{note}</p>}
      {others.length > 0 && (
        <ul className="mb-0 mt-2">
          {others.map(([field, message]) => (
            <li key={field}>
              <code>{field}</code>: {message}
            </li>
          ))}
        </ul>
      )}
    </Alert>
  )
}
