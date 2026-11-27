import type { Draft, DraftInputM2, JsonObject, MigrationMode } from '../../api/types'

export type Reversibility = 'down' | 'irreversible'

export type BatchForm = {
  schema: string
  table: string
  cursorColumn: string
  batchSize: string
  parameters: string
}

export type DraftForm = {
  title: string
  sql: string
  reversibility: Reversibility
  sqlDown: string
  irreversibleReason: string
  mode: MigrationMode
  batch: BatchForm
}

export const TITLE_MAX = 100
export const BATCH_SIZE_MAX = 10_000
const SQL_MAX_BYTES = 262_144

const EMPTY_BATCH: BatchForm = { schema: '', table: '', cursorColumn: '', batchSize: '1000', parameters: '{}' }

export const EMPTY_FORM: DraftForm = {
  title: '',
  sql: '',
  reversibility: 'down',
  sqlDown: '',
  irreversibleReason: '',
  mode: 'standard',
  batch: EMPTY_BATCH,
}

// Kunci field yang sedang tampil di form, sama dengan kunci field_errors server.
export function visibleFields(form: DraftForm): string[] {
  const fields = ['title', 'sql', 'mode', form.reversibility === 'down' ? 'sql_down' : 'irreversible_reason']
  if (form.mode === 'batch') {
    fields.push('batch', 'batch.schema', 'batch.table', 'batch.cursor_column', 'batch.batch_size', 'batch.parameters')
  }
  return fields
}

export function formFromDraft(draft: Draft): DraftForm {
  const batch = draft.batch ?? null
  return {
    title: draft.title,
    sql: draft.sql,
    reversibility: draft.sql_down == null && draft.irreversible_reason != null ? 'irreversible' : 'down',
    sqlDown: draft.sql_down ?? '',
    irreversibleReason: draft.irreversible_reason ?? '',
    mode: draft.mode ?? 'standard',
    batch: batch
      ? {
          schema: batch.schema,
          table: batch.table,
          cursorColumn: batch.cursor_column,
          batchSize: String(batch.batch_size),
          parameters: JSON.stringify(batch.parameters, null, 2),
        }
      : EMPTY_BATCH,
  }
}

function parseParameters(text: string): JsonObject | null {
  try {
    const value: unknown = JSON.parse(text)
    return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as JsonObject) : null
  } catch {
    return null
  }
}

const bytes = (s: string) => new TextEncoder().encode(s).length

export function validateDraft(form: DraftForm): Record<string, string> {
  const errors: Record<string, string> = {}
  const title = form.title.trim()
  if (title.length === 0) errors.title = 'Title wajib diisi.'
  else if (title.length > TITLE_MAX) errors.title = `Title maksimal ${TITLE_MAX} karakter.`
  if (form.sql.trim().length === 0) errors.sql = 'SQL up wajib diisi.'
  else if (bytes(form.sql) > SQL_MAX_BYTES) errors.sql = 'SQL up maksimal 262144 byte.'

  if (form.reversibility === 'down') {
    if (form.sqlDown.trim().length === 0) errors.sql_down = 'Isi SQL down, atau pilih "Tidak dapat dibatalkan".'
    else if (bytes(form.sqlDown) > SQL_MAX_BYTES) errors.sql_down = 'SQL down maksimal 262144 byte.'
  } else if (form.irreversibleReason.trim().length === 0) {
    errors.irreversible_reason = 'Isi alasan migration tidak dapat dibatalkan.'
  }

  if (form.mode === 'batch') {
    const b = form.batch
    if (!b.schema.trim()) errors['batch.schema'] = 'Schema wajib diisi.'
    if (!b.table.trim()) errors['batch.table'] = 'Table wajib diisi.'
    if (!b.cursorColumn.trim()) errors['batch.cursor_column'] = 'Kolom cursor wajib diisi.'
    const size = Number(b.batchSize)
    if (!/^\d+$/.test(b.batchSize.trim()) || size < 1 || size > BATCH_SIZE_MAX) {
      errors['batch.batch_size'] = `Ukuran batch bilangan bulat 1–${BATCH_SIZE_MAX}.`
    }
    if (parseParameters(b.parameters) === null) errors['batch.parameters'] = 'Parameter harus objek JSON, misalnya {}.'
  }
  return errors
}

export function toDraftInput(form: DraftForm): DraftInputM2 {
  const down = form.reversibility === 'down'
  return {
    title: form.title.trim(),
    sql: form.sql,
    sql_down: down ? form.sqlDown : null,
    irreversible_reason: down ? null : form.irreversibleReason.trim(),
    mode: form.mode,
    batch:
      form.mode === 'batch'
        ? {
            schema: form.batch.schema.trim(),
            table: form.batch.table.trim(),
            cursor_column: form.batch.cursorColumn.trim(),
            batch_size: Number(form.batch.batchSize),
            parameters: parseParameters(form.batch.parameters) ?? {},
          }
        : null,
  }
}

export function sameForm(a: DraftForm, b: DraftForm): boolean {
  return JSON.stringify(a) === JSON.stringify(b)
}
