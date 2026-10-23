import type { ReactNode } from 'react'

type Props = { title: string; children?: ReactNode }

export function EmptyState({ title, children }: Props) {
  return (
    <div className="empty py-4">
      <p className="empty-title">{title}</p>
      {children && <div className="empty-subtitle text-secondary">{children}</div>}
    </div>
  )
}
