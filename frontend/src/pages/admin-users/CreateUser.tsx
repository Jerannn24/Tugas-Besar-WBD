import { useState, type FormEvent } from 'react'
import { createUser } from '../../api/endpoints'
import type { User } from '../../api/types'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { CheckField } from '../../components/CheckField'
import { FormField } from '../../components/FormField'
import { MutationAlert } from '../../components/MutationAlert'
import { Panel } from '../../components/Panel'
import { describeFailure, type MutationFailure } from '../../lib/mutation'
import { USERNAME_MAX, passwordError, usernameError } from './rules'

const EMPTY = { username: '', password: '', is_admin: false }
const FIELDS = ['username', 'password', 'is_admin']

export function CreateUser({ onCreated }: { onCreated: (user: User) => void }) {
  const [form, setForm] = useState(EMPTY)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [failure, setFailure] = useState<MutationFailure | null>(null)
  const [created, setCreated] = useState<string | null>(null)
  const [pending, setPending] = useState(false)

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (pending) return
    setFailure(null)
    setCreated(null)
    const local: Record<string, string> = {}
    const u = usernameError(form.username)
    const p = passwordError(form.password)
    if (u) local.username = u
    if (p) local.password = p
    setErrors(local)
    if (Object.keys(local).length > 0) return

    setPending(true)
    const res = await createUser({ username: form.username.trim(), password: form.password, is_admin: form.is_admin })
    setPending(false)
    if (res.ok) {
      setForm(EMPTY)
      setCreated(res.data.username)
      onCreated(res.data)
      return
    }
    const described = describeFailure(res, 'User gagal dibuat')
    if (!described) return
    setErrors(described.fieldErrors)
    setFailure(described)
  }

  return (
    <Panel title="Buat user">
      <form method="post" noValidate onSubmit={handleSubmit}>
        <MutationAlert failure={failure} shownFields={FIELDS} />
        {created && (
          <Alert variant="success" title="User dibuat">
            User <strong>{created}</strong> berhasil dibuat.
          </Alert>
        )}
        <FormField label="Username" required error={errors.username}>
          {(control) => (
            <input
              {...control}
              className={`form-control${errors.username ? ' is-invalid' : ''}`}
              type="text"
              autoComplete="off"
              maxLength={USERNAME_MAX}
              value={form.username}
              disabled={pending}
              onChange={(e) => setForm({ ...form, username: e.target.value })}
            />
          )}
        </FormField>
        <FormField label="Password" required error={errors.password} hint="Minimal 12 karakter.">
          {(control) => (
            <input
              {...control}
              className={`form-control${errors.password ? ' is-invalid' : ''}`}
              type="password"
              autoComplete="new-password"
              value={form.password}
              disabled={pending}
              onChange={(e) => setForm({ ...form, password: e.target.value })}
            />
          )}
        </FormField>
        <CheckField
          label="Platform Admin"
          checked={form.is_admin}
          disabled={pending}
          error={errors.is_admin}
          hint="Admin mengelola user, group, dan connection; bukan level akses database."
          onChange={(is_admin) => setForm({ ...form, is_admin })}
        />
        <Button type="submit" pending={pending}>
          Buat user
        </Button>
      </form>
    </Panel>
  )
}
