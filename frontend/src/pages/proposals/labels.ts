import type { MigrationAction, Validation } from '../../api/types'

export const ACTION_LABELS: Record<MigrationAction, string> = {
  apply: 'Apply',
  rollback: 'Rollback',
  forward: 'Forward',
  resume: 'Resume',
  compensate: 'Kompensasi',
}

export function validationLabel(v: Validation | null): string {
  if (v === null) return 'Belum divalidasi'
  return v.status === 'passed' ? 'Lolos' : `Gagal (${v.issues.length})`
}
