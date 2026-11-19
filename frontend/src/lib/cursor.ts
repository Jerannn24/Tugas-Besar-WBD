import { useCallback, useEffect, useRef, useState } from 'react'
import type { ApiFailure, ApiResult } from '../api/client'
import type { CursorPage } from '../api/types'

export type CursorState<T> =
  | { status: 'loading' }
  | { status: 'failed'; failure: ApiFailure }
  | { status: 'ready'; items: T[]; next: string | null; more: boolean; moreFailure: ApiFailure | null }

type Loader<T> = (cursor: string | null) => Promise<ApiResult<CursorPage<T>>>

export function useCursorList<T>(loader: Loader<T>) {
  const [state, setState] = useState<CursorState<T>>({ status: 'loading' })
  const seq = useRef(0)

  const fetchFirst = useCallback(async () => {
    const mine = ++seq.current
    const res = await loader(null)
    if (mine !== seq.current) return
    setState(
      res.ok
        ? { status: 'ready', items: res.data.items, next: res.data.next_cursor, more: false, moreFailure: null }
        : { status: 'failed', failure: res },
    )
  }, [loader])

  const reload = useCallback(() => {
    setState({ status: 'loading' })
    return fetchFirst()
  }, [fetchFirst])

  useEffect(() => {
    void fetchFirst()
    const ref = seq
    return () => {
      ref.current++
    }
  }, [fetchFirst])

  const loadMore = useCallback(async () => {
    if (state.status !== 'ready' || state.next === null || state.more) return
    const current = state
    const mine = ++seq.current
    setState({ ...current, more: true, moreFailure: null })
    const res = await loader(current.next)
    if (mine !== seq.current) return
    if (res.ok) {
      setState({
        status: 'ready',
        items: [...current.items, ...res.data.items],
        next: res.data.next_cursor,
        more: false,
        moreFailure: null,
      })
    } else if (res.kind === 'http' && res.status === 400) {
      void reload()
    } else {
      setState({ ...current, more: false, moreFailure: res })
    }
  }, [state, loader, reload])

  return { state, reload, loadMore }
}
