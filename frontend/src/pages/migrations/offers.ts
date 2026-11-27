import type { HistoryAttemptRow, MigrationAction } from '../../api/types'

type Offer = {
  action: MigrationAction
  allowed: 'propose_rollback' | 'propose_forward' | 'propose_compensate'
  label: string
  danger?: boolean
}

export const OFFERS: Offer[] = [
  { action: 'rollback', allowed: 'propose_rollback', label: 'Ajukan rollback' },
  { action: 'forward', allowed: 'propose_forward', label: 'Ajukan forward' },
  { action: 'compensate', allowed: 'propose_compensate', label: 'Batalkan per batch', danger: true },
]

export const hasOffers = (row: HistoryAttemptRow): boolean =>
  row.revision !== null && OFFERS.some((o) => row.allowed_actions.includes(o.allowed))
