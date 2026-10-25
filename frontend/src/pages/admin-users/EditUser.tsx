import { useState, type FormEvent } from 'react'
import { updateUser } from '../../api/endpoints'
import type { User, UserPatchInput } from '../../api/types'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { CheckField } from '../../components/CheckField'
import { FormField } from '../../components/FormField'
import { MutationAlert } from '../../components/MutationAlert'
import { Panel } from '../../components/Panel'
import { describeFailure, type MutationFailure } from '../../lib/mutation'
import { useBeforeUnload } from '../../lib/unload'
import { USERNAME_MAX, passwordError, usernameError } from './rules'

const FIELDS = ['username', 'password', 'is_admin', 'is_active']

type Props = {
  user: User
  isSelf: boolean
  onSaved: (user: User) => void
  onClose: () => void
}

export function EditUser({ user, isSelf, onSaved, onClose }: Props) {
  const [form, setForm] = useState({ username: user.username, password: '', is_admin: user.is_admin, is_active: user.is_active })
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [failure, setFailure] = useState<MutationFailure | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [pending, setPending] = useState(false)

  const patch: UserPatchInput = {}
  if (form.username.trim() !== user.username) patch.username = form.username.trim()
  if (form.is_admin !== user.is_admin) patch.is_admin = form.is_admin
  if (form.is_active !== user.is_active) patch.is_active = form.is_active
  if (form.password !== '') patch.password = form.password
  const dirty = Object.keys(patch).length > 0
  useBeforeUnload(dirty)

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (pending) return
    setFailure(null)
    setNotice(null)
    if (!dirty) {
      setNotice('Tidak ada perubahan untuk disimpan.')
      return
    }
    const local: Record<string, string> = {}
    const u = patch.username !== undefined ? usernameError(form.username) : null
    const p = patch.password !== undefined ? passwordError(form.password) : null
    if (u) local.username = u
    if (p) local.password = p
    setErrors(local)
    if (Object.keys(local).length > 0) return

    setPending(true)
    const res = await updateUser(user.id, patch)
    if (res.ok) {
      if (isSelf) {
        window.location.assign('/admin/users/')
        return
      }
      setPending(false)
      setForm((f) => ({ ...f, password: '' }))
      onSaved(res.data)
      setNotice(`Perubahan untuk ${res.data.username} tersimpan.`)
      return
    }
    setPending(false)
    const described = describeFailure(res, 'Perubahan gagal disimpan')
    if (!described) return
    setErrors(described.fieldErrors)
    setFailure(described)
  }

  return (
    <Panel
      title={<>Edit user: <span className="mono">{user.username}</span></>}
      actions={
        <Button variant="link" onClick={onClose} disabled={pending}>
          Tutup
        </Button>
      }
    >
      <form method="post" noValidate onSubmit={handleSubmit}>
        <MutationAlert failure={failure} shownFields={FIELDS} />
        {notice && <Alert variant="info">{notice}</Alert>}
        {isSelf && (
          <Alert variant="warning">
            Ini akun Anda sendiri. Setelah disimpan halaman dimuat ulang; Anda mungkin perlu login lagi.
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
        <FormField label="Password baru" error={errors.password} hint="Minimal 12 karakter. Kosong = password tidak diubah.">
          {(control) => (
            <input
              {...control}
              className={`form-control${errors.password ? ' is-invalid' : ''}`}
              type="password"
              autoComplete="new-password"
              placeholder="Biarkan kosong jika tidak diubah"
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
          hint="Bukan level akses database; akses database tetap lewat grant group."
          onChange={(is_admin) => setForm({ ...form, is_admin })}
        />
        <CheckField
          label="Aktif"
          checked={form.is_active}
          disabled={pending}
          error={errors.is_active}
          hint="Menonaktifkan user mencabut session-nya. User tidak dihapus agar riwayat migration tetap utuh."
          onChange={(is_active) => setForm({ ...form, is_active })}
        />
        <div className="d-flex gap-2 align-items-center">
          <Button type="submit" pending={pending}>
            Simpan perubahan
          </Button>
          {dirty && !pending && <span className="text-secondary">Ada perubahan belum disimpan.</span>}
        </div>
      </form>
    </Panel>
  )
}
