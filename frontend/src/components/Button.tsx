import type { ButtonHTMLAttributes } from 'react'

type Variant = 'primary' | 'secondary' | 'danger' | 'success' | 'outline-secondary' | 'link'

type Props = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: Variant
  pending?: boolean
}

export function Button({
  variant = 'primary',
  pending = false,
  disabled,
  type = 'button',
  className = '',
  children,
  ...rest
}: Props) {
  return (
    <button
      {...rest}
      type={type}
      className={`btn btn-${variant} ${className}`.trim()}
      disabled={disabled || pending}
      aria-busy={pending || undefined}
    >
      {pending && <span className="spinner-border spinner-border-sm me-2" aria-hidden="true" />}
      {children}
    </button>
  )
}
