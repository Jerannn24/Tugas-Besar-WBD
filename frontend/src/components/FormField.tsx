import { useId, type ReactElement, type ReactNode } from 'react'

type ControlProps = {
  id: string
  'aria-invalid'?: boolean
  'aria-describedby'?: string
}

type Props = {
  label: string
  error?: string
  hint?: ReactNode
  required?: boolean
  children: (control: ControlProps) => ReactElement
}

export function FormField({ label, error, hint, required, children }: Props) {
  const id = useId()
  const hintId = `${id}-hint`
  const errorId = `${id}-error`
  const describedBy = [hint ? hintId : null, error ? errorId : null].filter(Boolean).join(' ')

  return (
    <div className="mb-3">
      <label className={`form-label${required ? ' required' : ''}`} htmlFor={id}>
        {label}
      </label>
      {children({
        id,
        'aria-invalid': error ? true : undefined,
        'aria-describedby': describedBy || undefined,
      })}
      {hint && (
        <small id={hintId} className="form-hint">
          {hint}
        </small>
      )}
      {error && (
        <div id={errorId} className="invalid-feedback d-block">
          {error}
        </div>
      )}
    </div>
  )
}
