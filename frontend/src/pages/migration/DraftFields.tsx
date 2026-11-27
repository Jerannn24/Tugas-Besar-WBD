import { useId } from 'react'
import type { MigrationMode } from '../../api/types'
import { FormField } from '../../components/FormField'
import { BATCH_SIZE_MAX, TITLE_MAX, type BatchForm, type DraftForm, type Reversibility } from './draft'

type Props = {
  value: DraftForm
  onChange: (value: DraftForm) => void
  errors: Record<string, string>
  disabled?: boolean
}

const invalid = (base: string, error?: string) => `${base}${error ? ' is-invalid' : ''}`

const SQL_PROPS = { spellCheck: false, autoCapitalize: 'off', autoCorrect: 'off' } as const

export function DraftFields({ value, onChange, errors, disabled }: Props) {
  const set = (patch: Partial<DraftForm>) => onChange({ ...value, ...patch })
  const setBatch = (patch: Partial<BatchForm>) => onChange({ ...value, batch: { ...value.batch, ...patch } })

  return (
    <>
      <FormField label="Title" required error={errors.title}>
        {(control) => (
          <input
            {...control}
            className={invalid('form-control', errors.title)}
            type="text"
            maxLength={TITLE_MAX}
            value={value.title}
            disabled={disabled}
            onChange={(e) => set({ title: e.target.value })}
          />
        )}
      </FormField>

      <ModeField value={value.mode} error={errors.mode} disabled={disabled} onChange={(mode) => set({ mode })} />

      <FormField
        label="SQL up"
        required
        error={errors.sql}
        hint="Dijalankan worker setelah proposal disetujui dan diantrekan."
      >
        {(control) => (
          <textarea
            {...control}
            {...SQL_PROPS}
            className={invalid('form-control sql-editor', errors.sql)}
            value={value.sql}
            disabled={disabled}
            onChange={(e) => set({ sql: e.target.value })}
          />
        )}
      </FormField>

      <ReversibilityField
        value={value.reversibility}
        disabled={disabled}
        onChange={(reversibility) => set({ reversibility })}
      />
      {value.reversibility === 'down' ? (
        <FormField
          label="SQL down"
          required
          error={errors.sql_down}
          hint="Membatalkan perubahan SQL up saat rollback."
        >
          {(control) => (
            <textarea
              {...control}
              {...SQL_PROPS}
              className={invalid('form-control sql-editor', errors.sql_down)}
              value={value.sqlDown}
              disabled={disabled}
              onChange={(e) => set({ sqlDown: e.target.value })}
            />
          )}
        </FormField>
      ) : (
        <FormField label="Alasan tidak dapat dibatalkan" required error={errors.irreversible_reason}>
          {(control) => (
            <textarea
              {...control}
              className={invalid('form-control', errors.irreversible_reason)}
              rows={3}
              value={value.irreversibleReason}
              disabled={disabled}
              onChange={(e) => set({ irreversibleReason: e.target.value })}
            />
          )}
        </FormField>
      )}

      {value.mode === 'batch' && (
        <BatchFields value={value.batch} errors={errors} disabled={disabled} onChange={setBatch} />
      )}
    </>
  )
}

function ModeField({
  value,
  error,
  disabled,
  onChange,
}: {
  value: MigrationMode
  error?: string
  disabled?: boolean
  onChange: (mode: MigrationMode) => void
}) {
  return (
    <FormField label="Mode" required error={error}>
      {(control) => (
        <select
          {...control}
          className={invalid('form-select', error)}
          value={value}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value as MigrationMode)}
        >
          <option value="standard">Standard (satu transaction)</option>
          <option value="batch">Batch (bertahap per batch)</option>
        </select>
      )}
    </FormField>
  )
}

function ReversibilityField({
  value,
  disabled,
  onChange,
}: {
  value: Reversibility
  disabled?: boolean
  onChange: (value: Reversibility) => void
}) {
  const name = useId()
  const options: { value: Reversibility; label: string }[] = [
    { value: 'down', label: 'Dapat dibatalkan (isi SQL down)' },
    { value: 'irreversible', label: 'Tidak dapat dibatalkan (isi alasan)' },
  ]
  return (
    <fieldset className="mb-3">
      <legend className="form-label required">Pembatalan</legend>
      {options.map((o) => (
        <label key={o.value} className="form-check">
          <input
            className="form-check-input"
            type="radio"
            name={name}
            value={o.value}
            checked={value === o.value}
            disabled={disabled}
            onChange={() => onChange(o.value)}
          />
          <span className="form-check-label">{o.label}</span>
        </label>
      ))}
    </fieldset>
  )
}

type BatchProps = {
  value: BatchForm
  errors: Record<string, string>
  disabled?: boolean
  onChange: (patch: Partial<BatchForm>) => void
}

function BatchFields({ value, errors, disabled, onChange }: BatchProps) {
  const text = (label: string, key: 'schema' | 'table' | 'cursorColumn', errorKey: string) => (
    <div className="col-12 col-md-6">
      <FormField label={label} required error={errors[errorKey]}>
        {(control) => (
          <input
            {...control}
            className={invalid('form-control', errors[errorKey])}
            type="text"
            value={value[key]}
            disabled={disabled}
            onChange={(e) => onChange({ [key]: e.target.value })}
          />
        )}
      </FormField>
    </div>
  )

  return (
    <fieldset className="border rounded p-3 mb-3">
      <legend className="form-label float-none w-auto px-1 mb-2">Konfigurasi batch</legend>
      <div className="alert alert-info" role="note">
        <p className="mb-2">Pola batch yang didukung kontrak:</p>
        <pre className="sql-block mb-2">
          {'UPDATE managed.batch_items\nSET processed = TRUE\nWHERE id = ANY($1::bigint[]);'}
        </pre>
        <p className="mb-0">
          SQL down memakai <code>processed = FALSE</code>. <code>$1</code> diisi worker dari scope yang tersimpan. SQL
          lain tidak dipecah otomatis menjadi batch.
        </p>
      </div>
      {errors.batch && <div className="invalid-feedback d-block mb-2">{errors.batch}</div>}
      <div className="row g-2">
        {text('Schema', 'schema', 'batch.schema')}
        {text('Table', 'table', 'batch.table')}
        {text('Kolom cursor', 'cursorColumn', 'batch.cursor_column')}
        <div className="col-12 col-md-6">
          <FormField label="Ukuran batch" required error={errors['batch.batch_size']} hint={`1–${BATCH_SIZE_MAX} baris`}>
            {(control) => (
              <input
                {...control}
                className={invalid('form-control', errors['batch.batch_size'])}
                type="number"
                inputMode="numeric"
                min={1}
                max={BATCH_SIZE_MAX}
                step={1}
                value={value.batchSize}
                disabled={disabled}
                onChange={(e) => onChange({ batchSize: e.target.value })}
              />
            )}
          </FormField>
        </div>
        <div className="col-12">
          <FormField label="Parameter (JSON object)" required error={errors['batch.parameters']}>
            {(control) => (
              <textarea
                {...control}
                {...SQL_PROPS}
                className={invalid('form-control mono', errors['batch.parameters'])}
                rows={3}
                value={value.parameters}
                disabled={disabled}
                onChange={(e) => onChange({ parameters: e.target.value })}
              />
            )}
          </FormField>
        </div>
      </div>
    </fieldset>
  )
}
