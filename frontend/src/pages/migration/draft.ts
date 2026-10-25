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
