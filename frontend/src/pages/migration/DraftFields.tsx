import { FormField } from '../../components/FormField'

export type DraftForm = { title: string; sql: string }

export const TITLE_MAX = 100
const SQL_MAX_BYTES = 256 * 1024

export function validateDraft(form: DraftForm): Record<string, string> {
  const errors: Record<string, string> = {}
  const title = form.title.trim()
  if (title.length === 0) errors.title = 'Title wajib diisi.'
  else if (title.length > TITLE_MAX) errors.title = `Title maksimal ${TITLE_MAX} karakter.`
  if (form.sql.trim().length === 0) errors.sql = 'SQL wajib diisi.'
  else if (new TextEncoder().encode(form.sql).length > SQL_MAX_BYTES) errors.sql = 'SQL maksimal 256 KiB.'
  return errors
}

type Props = {
  value: DraftForm
  onChange: (value: DraftForm) => void
  errors: Record<string, string>
  disabled?: boolean
}

export function DraftFields({ value, onChange, errors, disabled }: Props) {
  return (
    <>
      <FormField label="Title" required error={errors.title}>
        {(control) => (
          <input
            {...control}
            className={`form-control${errors.title ? ' is-invalid' : ''}`}
            type="text"
            maxLength={TITLE_MAX}
            value={value.title}
            disabled={disabled}
            onChange={(e) => onChange({ ...value, title: e.target.value })}
          />
        )}
      </FormField>
      <FormField label="SQL" required error={errors.sql} hint="Dijalankan dalam satu transaction saat Execute.">
        {(control) => (
          <textarea
            {...control}
            className={`form-control sql-editor${errors.sql ? ' is-invalid' : ''}`}
            spellCheck={false}
            autoCapitalize="off"
            autoCorrect="off"
            value={value.sql}
            disabled={disabled}
            onChange={(e) => onChange({ ...value, sql: e.target.value })}
          />
        )}
      </FormField>
    </>
  )
}
