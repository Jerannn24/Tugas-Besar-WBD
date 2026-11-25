import { useState } from 'react'
import { useNavigate } from 'react-router'
import type { ApiFailure } from '../../api/client'
import type { HistoryAttemptRow, ID, MigrationAction } from '../../api/types'
import { createProposal } from '../../api/workflow'
import { Button } from '../../components/Button'
import { WorkflowAlert } from '../../components/WorkflowAlert'
import { paths } from '../../lib/paths'
import { OFFERS } from './offers'

export function Propose({ databaseId, row }: { databaseId: ID; row: HistoryAttemptRow }) {
  const navigate = useNavigate()
  const [pending, setPending] = useState<MigrationAction | null>(null)
  const [failure, setFailure] = useState<ApiFailure | null>(null)
  const revision = row.revision
  if (revision === null) return null
  const offers = OFFERS.filter((o) => row.allowed_actions.includes(o.allowed))

  async function propose(action: MigrationAction) {
    if (pending || revision === null) return
    setPending(action)
    setFailure(null)
    const res = await createProposal(databaseId, {
      migration_id: row.migration_id,
      revision,
      action,
      source_execution_id: row.execution_id,
    })
    if (res.ok) {
      navigate(paths.proposal(databaseId, res.data.proposal_id))
      return
    }
    setPending(null)
    setFailure(res)
  }

  return (
    <>
      {offers.map((o) => (
        <Button
          key={o.action}
          variant="outline-primary"
          pending={pending === o.action}
          disabled={pending !== null && pending !== o.action}
          onClick={() => void propose(o.action)}
        >
          {o.label}
        </Button>
      ))}
      {failure && (
        <div className="w-100">
          <WorkflowAlert
            failure={failure}
            title="Proposal gagal dibuat"
            note="Muat ulang riwayat untuk melihat aksi yang masih tersedia."
          />
        </div>
      )}
    </>
  )
}
