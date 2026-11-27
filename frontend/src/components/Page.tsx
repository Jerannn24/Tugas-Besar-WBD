import { useEffect, type ReactNode } from 'react'
import { APP_NAME } from '../lib/app'

export function Page({ title, children }: { title: string; children: ReactNode }) {
  useEffect(() => {
    document.title = `${title} - ${APP_NAME}`
  }, [title])

  return (
    <>
      <h1 className="page-title mb-3">{title}</h1>
      {children}
    </>
  )
}
