import type { Validation } from '../../api/types'
import { Alert } from '../../components/Alert'
import { formatDateTime } from '../../lib/format'

export function ValidationResult({ validation }: { validation: Validation | null }) {
  if (validation === null) {
    return (
      <Alert variant="info" title="Belum divalidasi">
        Validasi sintaks dan izin berjalan saat proposal diajukan untuk review.
      </Alert>
    )
  }
  const checked = `Diperiksa ${formatDateTime(validation.checked_at)}.`
  if (validation.status === 'passed') {
    return (
      <Alert variant="success" title="Validasi lolos">
        {checked}
      </Alert>
    )
  }
  return (
    <Alert variant="danger" title="Validasi gagal">
      <p className="mb-2">{checked} Perbaiki draft lalu ajukan proposal baru.</p>
      {validation.issues.length > 0 ? (
        <ul className="mb-0">
          {validation.issues.map((issue, i) => (
            <li key={i} className="text-break">
              <code>{issue.code}</code>: {issue.message}
            </li>
          ))}
        </ul>
      ) : (
        <p className="mb-0">Server tidak menyertakan rincian masalah.</p>
      )}
    </Alert>
  )
}
