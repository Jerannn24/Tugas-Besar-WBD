import { useId, type ReactNode } from 'react'

type Props = {
  label: string
  checked: boolean
  onChange: (checked: boolean) => void
  disabled?: boolean
  error?: string
  hint?: ReactNode
}

export function CheckField({ label, checked, onChange, disabled, error, hint }: Props) {
  const id = useId()
  const errorId = `${id}-error`
  const hintId = `${id}-hint`
  const describedBy = [hint ? hintId : null, error ? errorId : null].filter(Boolean).join(' ')

  return (
    <div className="mb-3">
      <label className="form-check mb-0" htmlFor={id}>
        <input
          id={id}
          className={`form-check-input${error ? ' is-invalid' : ''}`}
          type="checkbox"
          checked={checked}
          disabled={disabled}
          aria-invalid={error ? true : undefined}
          aria-describedby={describedBy || undefined}
          onChange={(e) => onChange(e.target.checked)}
        />
        <span className="form-check-label">{label}</span>
      </label>
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
