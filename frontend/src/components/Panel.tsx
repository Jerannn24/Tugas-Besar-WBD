import type { ReactNode } from 'react'

type Props = { title?: ReactNode; actions?: ReactNode; children: ReactNode; className?: string }

export function Panel({ title, actions, children, className = '' }: Props) {
  return (
    <section className={`card mb-3 ${className}`.trim()}>
      {(title || actions) && (
        <div className="card-header">
          {title && <h2 className="card-title">{title}</h2>}
          {actions && <div className="card-actions">{actions}</div>}
        </div>
      )}
      <div className="card-body">{children}</div>
    </section>
  )
}
