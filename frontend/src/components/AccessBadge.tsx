import type { DatabaseSummary } from '../api/types'

const BADGES: Record<DatabaseSummary['access'], { label: string; className: string }> = {
  view: { label: 'View Only', className: 'badge bg-azure-lt' },
  edit: { label: 'View + Edit', className: 'badge bg-green-lt' },
}

export function AccessBadge({ access }: { access: DatabaseSummary['access'] }) {
  const badge = BADGES[access]
  return <span className={badge.className}>{badge.label}</span>
}
