import { useCallback, useEffect, useRef, useState } from 'react'
import type { ApiFailure, ApiResult } from '../../api/client'
import { getJob } from '../../api/jobs'
import type { ID, JobStatus, JobView } from '../../api/types'

export const POLL_MS = 2_000

const ACTIVE: ReadonlySet<JobStatus> = new Set(['queued', 'running', 'retry_wait'])
const RETRYABLE: ReadonlySet<JobStatus> = new Set(['partial', 'partially_reverted'])

export const shouldPoll = (job: JobView) =>
  ACTIVE.has(job.status) || (RETRYABLE.has(job.status) && job.next_retry_at !== null)

export type JobState =
  | { status: 'loading' }
  | { status: 'failed'; failure: ApiFailure }
  | { status: 'ready'; job: JobView; updatedAt: number; refreshing: boolean; pollFailure: ApiFailure | null }

// 4xx menghentikan polling; network/5xx dicoba lagi pada tick berikutnya
const isFatal = (f: ApiFailure) => f.kind === 'http' && f.status < 500

export function useJob(databaseId: ID, jobId: ID) {
  const [state, setState] = useState<JobState>({ status: 'loading' })
  const [stopped, setStopped] = useState(false)
  const seq = useRef(0)
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const latest = useRef<JobView | null>(null)
  const tick = useRef<() => void>(() => {})

  const apply = useCallback((res: ApiResult<JobView>) => {
    let poll: boolean
    if (res.ok) {
      latest.current = res.data
      setState({ status: 'ready', job: res.data, updatedAt: Date.now(), refreshing: false, pollFailure: null })
      poll = shouldPoll(res.data)
    } else if (latest.current === null) {
      setState({ status: 'failed', failure: res })
      poll = false
    } else {
      setState((s) => (s.status === 'ready' ? { ...s, refreshing: false, pollFailure: res } : s))
      poll = !isFatal(res) && shouldPoll(latest.current)
    }
    setStopped(!poll)
    if (poll && !document.hidden) timer.current = setTimeout(() => tick.current(), POLL_MS)
  }, [])

  const request = useCallback(async () => {
    const mine = ++seq.current
    clearTimeout(timer.current)
    const res = await getJob(databaseId, jobId)
    if (mine === seq.current) apply(res)
  }, [databaseId, jobId, apply])

  useEffect(() => {
    tick.current = () => void request()
    void request()
    const ref = seq
    const timerRef = timer
    const onVisible = () => {
      if (document.hidden) clearTimeout(timerRef.current)
      // tab terlihat lagi: baca ulang bila job belum final
      else if (latest.current === null || shouldPoll(latest.current)) void request()
    }
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      ref.current++
      clearTimeout(timerRef.current)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [request])

  const refresh = useCallback(() => {
    setState((s) => (s.status === 'ready' ? { ...s, refreshing: true } : { status: 'loading' }))
    return request()
  }, [request])

  return { state, refresh, polling: state.status === 'ready' && !stopped }
}
