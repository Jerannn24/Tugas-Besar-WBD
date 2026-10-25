import { FormField } from '../../components/FormField'
import { TITLE_MAX, type DraftForm } from './draft'

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
