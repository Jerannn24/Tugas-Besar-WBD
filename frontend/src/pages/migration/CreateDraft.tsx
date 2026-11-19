import { useEffect, useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router'
import { errorMessage } from '../../api/client'
import { createDraft } from '../../api/endpoints'
import type { DatabaseSummary } from '../../api/types'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { LeaveGuard } from '../../components/LeaveGuard'
import { Panel } from '../../components/Panel'
import { paths } from '../../lib/paths'
import { validateDraft, type DraftForm } from './draft'
import { DraftFields } from './DraftFields'

export function CreateDraft({ database }: { database: DatabaseSummary }) {
  const [form, setForm] = useState<DraftForm>({ title: '', sql: '' })
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [message, setMessage] = useState<{ title: string; text: string } | null>(null)
  const [pending, setPending] = useState(false)
  const [createdId, setCreatedId] = useState<string | null>(null)
  const navigate = useNavigate()

  const dirty = form.title !== '' || form.sql !== ''

  useEffect(() => {
    if (createdId !== null) navigate(paths.draft(database.id, createdId), { replace: true })
  }, [createdId, database.id, navigate])

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (pending) return
    setMessage(null)
    const local = validateDraft(form)
    setErrors(local)
    if (Object.keys(local).length > 0) return

    setPending(true)
    const res = await createDraft(database.id, { title: form.title.trim(), sql: form.sql })
    if (res.ok) {
      setCreatedId(res.data.id)
      return
    }
    setPending(false)
    if (res.kind === 'http' && res.status === 401) return // client mengalihkan ke /login
    if (res.kind === 'http' && res.error.code === 'VALIDATION_FAILED') setErrors(res.error.field_errors)
    setMessage({ title: res.kind === 'http' && res.status === 403 ? 'Akses ditolak' : 'Draft gagal disimpan', text: errorMessage(res) })
  }

  return (
    <Panel title="Migration baru">
      <LeaveGuard when={dirty && createdId === null} />
      <form method="post" noValidate onSubmit={handleSubmit}>
        {message && (
          <Alert variant="danger" title={message.title}>
            {message.text}
          </Alert>
        )}
        <DraftFields value={form} onChange={setForm} errors={errors} disabled={pending} />
        <div className="d-flex flex-wrap gap-2 align-items-center">
          <Button type="submit" pending={pending}>
            Simpan draft
          </Button>
        </div>
      </form>
    </Panel>
  )
}
