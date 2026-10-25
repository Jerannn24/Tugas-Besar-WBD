import { useCallback, useEffect, useRef, useState } from 'react'
import type { ApiFailure, ApiResult } from '../api/client'

export type LoadState<T> =
  | { status: 'loading' }
  | { status: 'ready'; data: T }
  | { status: 'failed'; failure: ApiFailure }

export function useLoad<T>(loader: () => Promise<ApiResult<T>>) {
  const [state, setState] = useState<LoadState<T>>({ status: 'loading' })

  const seq = useRef(0)

  const fetchData = useCallback(async () => {
    const mine = ++seq.current
    const res = await loader()
    if (mine !== seq.current) return
    setState(res.ok ? { status: 'ready', data: res.data } : { status: 'failed', failure: res })
  }, [loader])

  const reload = useCallback(async () => {
    setState({ status: 'loading' })
    await fetchData()
  }, [fetchData])

  useEffect(() => {
    void fetchData()
    
    const ref = seq
    return () => {
      ref.current++
    }
  }, [fetchData])

  const update = useCallback((fn: (data: T) => T) => {
    setState((s) => (s.status === 'ready' ? { status: 'ready', data: fn(s.data) } : s))
  }, [])

  return { state, reload, update }
}

export const ADMIN_FORBIDDEN = 'Halaman ini hanya untuk Admin.'
