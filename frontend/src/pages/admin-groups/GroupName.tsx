import { useState, type FormEvent } from 'react'
import type { ApiResult } from '../../api/client'
import type { Group } from '../../api/types'
import { Button } from '../../components/Button'
import { FormField } from '../../components/FormField'
import { MutationAlert } from '../../components/MutationAlert'
import { describeFailure, type MutationFailure } from '../../lib/mutation'

export const GROUP_NAME_MAX = 100

type Props = {
  initial?: string
  label: string
  submitLabel: string
  failureTitle: string
  submit: (name: string) => Promise<ApiResult<Group>>
  onDone: (group: Group) => void
}

export function GroupName({ initial = '', label, submitLabel, failureTitle, submit, onDone }: Props) {
  const [name, setName] = useState(initial)
  const [error, setError] = useState<string | undefined>()
  const [failure, setFailure] = useState<MutationFailure | null>(null)
  const [pending, setPending] = useState(false)

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (pending) return
    setFailure(null)
    const trimmed = name.trim()
    if (trimmed.length === 0 || [...trimmed].length > GROUP_NAME_MAX) {
      setError(`Nama group harus 1 sampai ${GROUP_NAME_MAX} karakter.`)
      return
    }
    if (trimmed === initial) {
      setError('Nama belum diubah.')
      return
    }
    setError(undefined)
    setPending(true)
    const res = await submit(trimmed)
    setPending(false)
    if (res.ok) {
      setName(initial === '' ? '' : res.data.name)
      onDone(res.data)
      return
    }
    const described = describeFailure(res, failureTitle)
    if (!described) return
    setError(described.fieldErrors.name)
    setFailure(described)
  }

  return (
    <form method="post" noValidate onSubmit={handleSubmit}>
      <MutationAlert failure={failure} shownFields={['name']} />
      <FormField label={label} required error={error}>
        {(control) => (
          <input
            {...control}
            className={`form-control${error ? ' is-invalid' : ''}`}
            type="text"
            autoComplete="off"
            maxLength={GROUP_NAME_MAX}
            value={name}
            disabled={pending}
            onChange={(e) => setName(e.target.value)}
          />
        )}
      </FormField>
      <Button type="submit" pending={pending}>
        {submitLabel}
      </Button>
    </form>
  )
}
