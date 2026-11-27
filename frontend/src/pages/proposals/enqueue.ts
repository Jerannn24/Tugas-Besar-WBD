import { useCallback, useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router'
import type { ApiFailure } from '../../api/client'
import type { ID } from '../../api/types'
import { enqueueProposal, isEnqueueReplay, newRequestId } from '../../api/workflow'
import { classifyWorkflowFailure, type WorkflowFailureKind } from '../../lib/failure'
import { paths } from '../../lib/paths'

type Intent = { requestId: string; revision: number }

export type EnqueueState =
  | { phase: 'idle' }
  | { phase: 'confirming' }
  | { phase: 'posting' }
  | { phase: 'failed'; failure: ApiFailure; resend: boolean }

export type JobRouteState = { replayed: boolean }

const KEEP_INTENT: ReadonlySet<WorkflowFailureKind> = new Set([
  'network',
  'invalid_response',
  'queue_unavailable',
  'csrf',
  'server',
  'target_unavailable',
])

type Options = { databaseId: ID; proposalId: ID; onReload: () => void }

export function useEnqueue({ databaseId, proposalId, onReload }: Options) {
  const [state, setState] = useState<EnqueueState>({ phase: 'idle' })
  const intent = useRef<Intent | null>(null)
  const seq = useRef(0)
  const navigate = useNavigate()

  useEffect(() => {
    const ref = seq
    return () => {
      ref.current++
    }
  }, [])

  const post = useCallback(
    async (current: Intent) => {
      const mine = ++seq.current
      setState({ phase: 'posting' })
      const res = await enqueueProposal(databaseId, proposalId, {
        request_id: current.requestId,
        revision: current.revision,
      })
      if (mine !== seq.current) return

      if (res.ok) {
        intent.current = null
        const routeState: JobRouteState = { replayed: isEnqueueReplay(res) }
        navigate(paths.job(databaseId, res.data.job_id), { state: routeState })
        return
      }

      const w = classifyWorkflowFailure(res)
      const keep = KEEP_INTENT.has(w.kind)
      if (!keep) intent.current = null
      setState({ phase: 'failed', failure: res, resend: keep })
      if (w.reload || w.kind === 'forbidden' || w.kind === 'not_found') onReload()
    },
    [databaseId, proposalId, navigate, onReload],
  )

  const open = useCallback(() => setState({ phase: 'confirming' }), [])
  const cancel = useCallback(() => setState({ phase: 'idle' }), [])

  const confirm = useCallback(
    (revision: number) => {
      intent.current = { requestId: newRequestId(), revision }
      void post(intent.current)
    },
    [post],
  )

  const resend = useCallback(() => {
    if (intent.current) void post(intent.current)
  }, [post])

  return { state, open, cancel, confirm, resend, hasIntent: state.phase === 'failed' && state.resend }
}
