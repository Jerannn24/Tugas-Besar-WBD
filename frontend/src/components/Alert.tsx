import type { ReactNode } from 'react'

type Variant = 'danger' | 'warning' | 'success' | 'info'

type Props = { variant?: Variant; title?: string; children?: ReactNode }

export function Alert({ variant = 'info', title, children }: Props) {
  const role = variant === 'danger' || variant === 'warning' ? 'alert' : 'status'
  return (
    <div className={`alert alert-${variant}`} role={role}>
      {title && <h4 className="alert-title">{title}</h4>}
      {children && <div className="text-secondary-emphasis">{children}</div>}
    </div>
  )
}
