import type { Direction, JobStatus, MigrationAction, ProposalStatus, StatementOutcome } from '../api/types'

type Tone = 'secondary' | 'blue' | 'green' | 'red' | 'yellow' | 'orange' | 'purple'

const PROPOSAL: Record<ProposalStatus, [string, Tone]> = {
  draft: ['Draft', 'secondary'],
  pending_review: ['Menunggu approval', 'yellow'],
  approved: ['Disetujui', 'green'],
  queued: ['Diantrekan', 'blue'],
  superseded: ['Usang', 'secondary'],
}

const JOB: Record<JobStatus, [string, Tone]> = {
  queued: ['Queued', 'blue'],
  running: ['Running', 'purple'],
  retry_wait: ['Menunggu retry', 'yellow'],
  succeeded: ['Succeeded', 'green'],
  failed: ['Failed', 'red'],
  partial: ['Partial', 'orange'],
  partially_reverted: ['Partially reverted', 'orange'],
  unknown: ['Unknown', 'yellow'],
}

const ACTION: Record<MigrationAction, string> = {
  apply: 'Apply',
  rollback: 'Rollback',
  forward: 'Forward',
  resume: 'Resume',
  compensate: 'Kompensasi',
}

const OUTCOME: Record<StatementOutcome, [string, Tone]> = {
  succeeded: ['Succeeded', 'green'],
  failed: ['Failed', 'red'],
  unknown: ['Unknown', 'yellow'],
}

function Badge({ entry, raw }: { entry: [string, Tone] | undefined; raw: string }) {
  const [label, tone] = entry ?? [raw, 'secondary']
  return <span className={`badge bg-${tone}-lt`}>{label}</span>
}

export const ProposalBadge = ({ status }: { status: ProposalStatus }) => <Badge entry={PROPOSAL[status]} raw={status} />
export const JobBadge = ({ status }: { status: JobStatus }) => <Badge entry={JOB[status]} raw={status} />

export const AttemptBadge = ({ status }: { status: string }) => (
  <Badge entry={(JOB as Record<string, [string, Tone]>)[status]} raw={status} />
)

export const ActionBadge = ({ action, direction }: { action: MigrationAction; direction: Direction }) => (
  <Badge entry={[`${ACTION[action] ?? action} · ${direction}`, direction === 'down' ? 'orange' : 'blue']} raw={action} />
)
export const OutcomeBadge = ({ outcome }: { outcome: StatementOutcome }) => <Badge entry={OUTCOME[outcome]} raw={outcome} />
