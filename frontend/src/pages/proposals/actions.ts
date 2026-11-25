import { useCallback, useEffect, useRef, useState } from 'react'
import { canonicalCode, type ApiFailure, type ApiResult } from '../../api/client'
import type { ID, Proposal } from '../../api/types'
import { approveProposal, submitReview } from '../../api/workflow'
import { classifyWorkflowFailure } from '../../lib/failure'

export type Busy = { kind: 'submit' } | { kind: 'approve'; groupId: ID } | null
export type ActionResult = { failure: ApiFailure; title: string } | { success: string } | null

type Options = { databaseId: ID; proposalId: ID; onProposal: (p: Proposal) => void; onReload: () => void }
type RunOptions = { alwaysReload?: boolean; asLeaderRequired?: boolean }

function leaderRequired(res: ApiFailure): ApiFailure {
  if (res.kind !== 'http' || res.status !== 403 || canonicalCode(res.error.code) !== 'FORBIDDEN') return res
  return { ...res, error: { ...res.error, code: 'LEADER_REQUIRED', message: 'Hanya leader group yang dapat menyetujui proposal ini.' } }
}

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
    async (next: Busy, title: string, success: string, call: () => Promise<ApiResult<Proposal>>, opts: RunOptions = {}) => {
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
      const failure = opts.asLeaderRequired ? leaderRequired(res) : res
      setResult({ failure, title })
      const w = classifyWorkflowFailure(failure)
      if (opts.alwaysReload || w.reload || w.kind === 'validation' || w.kind === 'forbidden' || w.kind === 'not_found') onReload()
    },
    [onProposal, onReload],
  )

  const submit = useCallback(
    (revision: number) =>
      run({ kind: 'submit' }, 'Review gagal diajukan', 'Validasi lolos. Proposal menunggu approval.', () =>
        submitReview(databaseId, proposalId, { revision }),
        // Hasil validasi tersimpan walau submit ditolak, jadi proposal selalu dimuat ulang.
        { alwaysReload: true },
      ),
    [run, databaseId, proposalId],
  )

  const approve = useCallback(
    (revision: number, groupId: ID) =>
      run({ kind: 'approve', groupId }, 'Approval gagal', 'Approval tercatat.', () =>
        approveProposal(databaseId, proposalId, { revision, group_id: groupId }),
        { asLeaderRequired: true },
      ),
    [run, databaseId, proposalId],
  )

  return { busy, result, submit, approve }
}
