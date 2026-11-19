import { useCallback, useEffect, useRef, useState } from 'react'
import type { ApiFailure, ApiResult } from '../../api/client'
import type { ID, Proposal } from '../../api/types'
import { approveProposal, submitReview } from '../../api/workflow'
import { classifyWorkflowFailure } from '../../lib/failure'

export type Busy = { kind: 'submit' } | { kind: 'approve'; groupId: ID } | null
export type ActionResult = { failure: ApiFailure; title: string } | { success: string } | null

type Options = { databaseId: ID; proposalId: ID; onProposal: (p: Proposal) => void; onReload: () => void }

export function useProposalActions({ databaseId, proposalId, onProposal, onReload }: Options) {
  const [busy, setBusy] = useState<Busy>(null)
  const [result, setResult] = useState<ActionResult>(null)
  const seq = useRef(0)

  useEffect(() => {
    const ref = seq
    return () => {
      ref.current++
    }
  }, [])

  const run = useCallback(
    async (next: Busy, title: string, success: string, call: () => Promise<ApiResult<Proposal>>) => {
      const mine = ++seq.current
      setBusy(next)
      setResult(null)
      const res = await call()
      if (mine !== seq.current) return
      setBusy(null)
      if (res.ok) {
        onProposal(res.data)
        setResult({ success })
        return
      }
      setResult({ failure: res, title })
      const w = classifyWorkflowFailure(res)
      if (w.reload || w.kind === 'validation' || w.kind === 'forbidden' || w.kind === 'not_found') onReload()
    },
    [onProposal, onReload],
  )

  const submit = useCallback(
    (revision: number) =>
      run({ kind: 'submit' }, 'Review gagal diajukan', 'Validasi lolos. Proposal menunggu approval.', () =>
        submitReview(databaseId, proposalId, { revision }),
      ),
    [run, databaseId, proposalId],
  )

  const approve = useCallback(
    (revision: number, groupId: ID) =>
      run({ kind: 'approve', groupId }, 'Approval gagal', 'Approval tercatat.', () =>
        approveProposal(databaseId, proposalId, { revision, group_id: groupId }),
      ),
    [run, databaseId, proposalId],
  )

  return { busy, result, submit, approve }
}
