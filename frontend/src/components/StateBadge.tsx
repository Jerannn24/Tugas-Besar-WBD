import type { JobStatus, ProposalStatus } from '../api/types'

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

function Badge({ entry, raw }: { entry: [string, Tone] | undefined; raw: string }) {
  const [label, tone] = entry ?? [raw, 'secondary']
  return <span className={`badge bg-${tone}-lt`}>{label}</span>
}

export const ProposalBadge = ({ status }: { status: ProposalStatus }) => <Badge entry={PROPOSAL[status]} raw={status} />
export const JobBadge = ({ status }: { status: JobStatus }) => <Badge entry={JOB[status]} raw={status} />
