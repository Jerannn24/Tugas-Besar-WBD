import { useEffect, useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router'
import type { ApiFailure } from '../../api/client'
import type { DatabaseSummary } from '../../api/types'
import { createMigrationDraft } from '../../api/workflow'
import { Button } from '../../components/Button'
import { LeaveGuard } from '../../components/LeaveGuard'
import { Panel } from '../../components/Panel'
import { WorkflowAlert } from '../../components/WorkflowAlert'
import { paths } from '../../lib/paths'
import { EMPTY_FORM, sameForm, toDraftInput, validateDraft, visibleFields, type DraftForm } from './draft'
import { DraftFields } from './DraftFields'

export function CreateDraft({ database }: { database: DatabaseSummary }) {
  const [form, setForm] = useState<DraftForm>(EMPTY_FORM)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [failure, setFailure] = useState<ApiFailure | null>(null)
  const [pending, setPending] = useState(false)
  const [createdId, setCreatedId] = useState<string | null>(null)
  const navigate = useNavigate()

  const dirty = !sameForm(form, EMPTY_FORM)

  useEffect(() => {
    if (createdId !== null) navigate(paths.draft(database.id, createdId), { replace: true })
  }, [createdId, database.id, navigate])

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (pending) return
    setFailure(null)
    const local = validateDraft(form)
    setErrors(local)
    if (Object.keys(local).length > 0) return

    setPending(true)
    const res = await createMigrationDraft(database.id, toDraftInput(form))
    if (res.ok) {
      setCreatedId(res.data.id)
      return
    }
    setPending(false)
    if (res.kind === 'http') setErrors(res.error.field_errors)
    setFailure(res)
  }

  return (
    <Panel title="Migration baru">
      <LeaveGuard when={dirty && createdId === null} />
      <form method="post" noValidate onSubmit={handleSubmit}>
        <WorkflowAlert failure={failure} title="Draft gagal disimpan" shownFields={visibleFields(form)} />
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
