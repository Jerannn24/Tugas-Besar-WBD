import type { HistoryAttemptRow, MigrationAction } from '../../api/types'

export const OFFERS: { action: MigrationAction; allowed: 'propose_rollback' | 'propose_forward'; label: string }[] = [
  { action: 'rollback', allowed: 'propose_rollback', label: 'Ajukan rollback' },
  { action: 'forward', allowed: 'propose_forward', label: 'Ajukan forward' },
]

export const hasOffers = (row: HistoryAttemptRow): boolean =>
  row.revision !== null && OFFERS.some((o) => row.allowed_actions.includes(o.allowed))
