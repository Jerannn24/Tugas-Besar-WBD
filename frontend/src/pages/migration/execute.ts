import { useCallback, useEffect, useRef, useState } from 'react'
import type { ApiFailure } from '../../api/client'
import { executeDraft, getMigration } from '../../api/endpoints'
import type { Execution, ID } from '../../api/types'

const POLL_INTERVAL_MS = 2_000
const POLL_LIMIT_MS = 60_000

type Intent = { requestId: string; revision: number }

export type ExecuteState =
  | { phase: 'idle' }
  | { phase: 'confirming' }
  | { phase: 'posting' }
  | { phase: 'polling'; execution: Execution }
  | { phase: 'waiting'; execution: Execution; checking: boolean; checkFailure: ApiFailure | null }
  | { phase: 'final'; execution: Execution }
  | { phase: 'uncertain'; failure: ApiFailure }
  | { phase: 'rejected'; failure: ApiFailure }

type Options = {
  databaseId: ID
  draftId: ID
  onFinal: (execution: Execution) => void
  onRejected: (failure: ApiFailure) => void
}

export const isUnresolved = (x: Execution) => x.status === 'running' || x.status === 'unknown'

export function useExecute({ databaseId, draftId, onFinal, onRejected }: Options) {
  const [state, setState] = useState<ExecuteState>({ phase: 'idle' })
  const intentRef = useRef<Intent | null>(null)
  const seq = useRef(0)
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const callbacks = useRef({ onFinal, onRejected })

  useEffect(() => {
    callbacks.current = { onFinal, onRejected }
  }, [onFinal, onRejected])

  useEffect(() => {
    const seqRef = seq
    const timerRef = timer
    return () => {
      seqRef.current++
      clearTimeout(timerRef.current)
    }
  }, [])

  const finish = useCallback((execution: Execution) => {
    intentRef.current = null
    setState({ phase: 'final', execution })
    callbacks.current.onFinal(execution)
  }, [])

  const poll = useCallback(
    (first: Execution, deadline: number, mine: number) => {
      const tick = (execution: Execution) => {
        timer.current = setTimeout(async () => {
          const res = await getMigration(databaseId, execution.id)
          if (mine !== seq.current) return
          if (res.ok && !isUnresolved(res.data)) {
            finish(res.data)
            return
          }
          const latest = res.ok ? res.data : execution
          const fatal = !res.ok && res.kind === 'http' && res.status < 500
          if (fatal || Date.now() + POLL_INTERVAL_MS > deadline) {
            setState({ phase: 'waiting', execution: latest, checking: false, checkFailure: res.ok ? null : res })
            return
          }
          setState({ phase: 'polling', execution: latest })
          tick(latest)
        }, POLL_INTERVAL_MS)
      }
      tick(first)
    },
    [databaseId, finish],
  )

  const post = useCallback(
    async (intent: Intent) => {
      const mine = ++seq.current
      clearTimeout(timer.current)
      setState({ phase: 'posting' })
      const res = await executeDraft(databaseId, draftId, { revision: intent.revision, request_id: intent.requestId })
      if (mine !== seq.current) return

      if (res.ok) {
        if (!isUnresolved(res.data)) {
          finish(res.data)
        } else {
          setState({ phase: 'polling', execution: res.data })
          poll(res.data, Date.now() + POLL_LIMIT_MS, mine)
        }
        return
      }

      if (res.kind === 'http' && res.status < 500) {
        intentRef.current = null
        setState({ phase: 'rejected', failure: res })
        callbacks.current.onRejected(res)
        return
      }

      setState({ phase: 'uncertain', failure: res })
    },
    [databaseId, draftId, finish, poll],
  )

  const open = useCallback(() => setState({ phase: 'confirming' }), [])
  const cancel = useCallback(() => setState({ phase: 'idle' }), [])

  const confirm = useCallback(
    (savedRevision: number) => {
      intentRef.current = { requestId: crypto.randomUUID(), revision: savedRevision }
      void post(intentRef.current)
    },
    [post],
  )

  const recheck = useCallback(() => {
    if (intentRef.current) void post(intentRef.current)
  }, [post])

  const checkStatus = useCallback(async () => {
    if (state.phase !== 'waiting') return
    const execution = state.execution
    const mine = ++seq.current
    setState({ ...state, checking: true, checkFailure: null })
    const res = await getMigration(databaseId, execution.id)
    if (mine !== seq.current) return
    if (res.ok && !isUnresolved(res.data)) finish(res.data)
    else setState({ phase: 'waiting', execution: res.ok ? res.data : execution, checking: false, checkFailure: res.ok ? null : res })
  }, [databaseId, finish, state])

  return { state, open, cancel, confirm, recheck, checkStatus }
}
