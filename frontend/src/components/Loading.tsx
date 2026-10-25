type Props = { label?: string }

export function Loading({ label = 'Memuat…' }: Props) {
  return (
    <div className="d-flex align-items-center gap-2 py-3 text-secondary" role="status" aria-live="polite">
      <span className="spinner-border spinner-border-sm" aria-hidden="true" />
      <span>{label}</span>
    </div>
  )
}
