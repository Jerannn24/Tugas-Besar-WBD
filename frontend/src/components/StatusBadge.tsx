import type { ExecutionStatus } from '../api/types'

const LABELS: Record<ExecutionStatus, string> = {
  running: 'Running',
  succeeded: 'Succeeded',
  failed: 'Failed',
  unknown: 'Unknown',
}

export function StatusBadge({ status }: { status: ExecutionStatus }) {
  return <span className={`status-badge status-badge--${status}`}>{LABELS[status]}</span>
}
