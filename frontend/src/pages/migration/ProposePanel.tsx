import { useState } from 'react'
import { Link, useNavigate } from 'react-router'
import { isErrorCode, type ApiFailure } from '../../api/client'
import type { DatabaseSummary, Draft } from '../../api/types'
import { createProposal } from '../../api/workflow'
import { Button } from '../../components/Button'
import { Panel } from '../../components/Panel'
import { WorkflowAlert } from '../../components/WorkflowAlert'
import { paths } from '../../lib/paths'

type Props = {
  database: DatabaseSummary
  draft: Draft
  blockedReason: string | null
  onConflict: () => void
}

export function ProposePanel({ database, draft, blockedReason, onConflict }: Props) {
  const [pending, setPending] = useState(false)
  const [failure, setFailure] = useState<ApiFailure | null>(null)
  const navigate = useNavigate()

  const reason = draft.applied_execution_id !== null ? 'Draft sudah applied.' : blockedReason

  async function propose() {
    if (pending) return
    setFailure(null)
    setPending(true)
    const res = await createProposal(database.id, {
      migration_id: draft.id,
      revision: draft.revision,
      action: 'apply',
      source_execution_id: null,
    })
    if (res.ok) {
      navigate(paths.proposal(database.id, res.data.proposal_id))
      return
    }
    setPending(false)
    setFailure(res)
    if (isErrorCode(res, 'VERSION_CONFLICT')) onConflict()
  }

  return (
    <Panel title="Ajukan proposal">
      <p className="text-secondary">
        Eksekusi migration selalu melalui proposal: validasi, approval leader group, lalu antrean. Proposal memakai
        snapshot revision tersimpan <strong>{draft.revision}</strong>.
      </p>
      <WorkflowAlert failure={failure} title="Proposal gagal dibuat" />
      <div className="d-flex flex-wrap align-items-center gap-3">
        <Button
          disabled={reason !== null}
          pending={pending}
          onClick={() => void propose()}
          aria-describedby={reason ? 'propose-reason' : undefined}
        >
          Ajukan proposal revision {draft.revision}
        </Button>
        {reason && (
          <span id="propose-reason" className="text-secondary">
            {reason}
          </span>
        )}
        <Link to={paths.proposals(database.id)}>Lihat semua proposal</Link>
      </div>
    </Panel>
  )
}
