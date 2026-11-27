import { useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router'
import type { ApiFailure } from '../../api/client'
import type { JobView } from '../../api/types'
import { createProposal } from '../../api/workflow'
import { Button } from '../../components/Button'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { Panel } from '../../components/Panel'
import { WorkflowAlert } from '../../components/WorkflowAlert'
import { classifyWorkflowFailure } from '../../lib/failure'
import { paths } from '../../lib/paths'

type Kind = 'resume' | 'compensate'

const TEXT: Record<Kind, { label: string; confirm: string; body: string }> = {
  resume: {
    label: 'Ajukan resume',
    confirm: 'Ya, buat proposal resume',
    body: 'Resume melanjutkan batch yang belum committed pada execution yang sama. Proposal baru tetap perlu review dan approval.',
  },
  compensate: {
    label: 'Ajukan kompensasi',
    confirm: 'Ya, buat proposal kompensasi',
    body: 'Kompensasi membatalkan batch yang sudah committed memakai SQL down, sebagai execution baru. Proposal baru tetap perlu review dan approval.',
  },
}

type Props = { job: JobView; onReload: () => void }

export function JobActions({ job, onReload }: Props) {
  const [confirm, setConfirm] = useState<Kind | null>(null)
  const [pending, setPending] = useState<Kind | null>(null)
  const [failure, setFailure] = useState<ApiFailure | null>(null)
  const navigate = useNavigate()
  const alive = useRef(true)

  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])

  const kinds = (['resume', 'compensate'] as const).filter((k) => job.allowed_actions.includes(`propose_${k}`))
  if (kinds.length === 0) return null

  async function propose(kind: Kind) {
    setConfirm(null)
    setFailure(null)
    setPending(kind)
    const res = await createProposal(job.database_id, {
      migration_id: job.migration_id,
      revision: job.revision,
      action: kind,
      source_execution_id: job.execution_id,
    })
    if (!alive.current) return
    if (res.ok) {
      navigate(paths.proposal(job.database_id, res.data.proposal_id))
      return
    }
    setPending(null)
    setFailure(res)
    if (classifyWorkflowFailure(res).reload) onReload()
  }

  return (
    <Panel title="Pemulihan">
      <WorkflowAlert failure={failure} title="Proposal gagal dibuat" />
      <div className="d-flex flex-wrap gap-2">
        {kinds.map((k) => (
          <Button
            key={k}
            variant={k === 'compensate' ? 'outline-danger' : 'primary'}
            pending={pending === k}
            disabled={pending !== null}
            onClick={() => setConfirm(k)}
          >
            {TEXT[k].label}
          </Button>
        ))}
      </div>
      <ConfirmDialog
        open={confirm !== null}
        title={confirm ? TEXT[confirm].label : ''}
        confirmLabel={confirm ? TEXT[confirm].confirm : undefined}
        danger={confirm === 'compensate'}
        onCancel={() => setConfirm(null)}
        onConfirm={() => confirm && void propose(confirm)}
      >
        {confirm && TEXT[confirm].body}
      </ConfirmDialog>
    </Panel>
  )
}
