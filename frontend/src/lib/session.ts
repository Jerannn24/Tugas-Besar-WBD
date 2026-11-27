import { createContext, useContext } from 'react'
import type { SessionView } from '../api/types'

export const SessionContext = createContext<SessionView | null>(null)

export function useSession(): SessionView {
  const session = useContext(SessionContext)
  if (session === null) throw new Error('useSession dipanggil di luar Shell')
  return session
}
