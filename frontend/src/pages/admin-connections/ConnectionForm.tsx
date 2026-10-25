import { useState, type FormEvent } from 'react'
import { createConnection, updateConnection } from '../../api/endpoints'
import type { ConnectionInput, ConnectionPatchInput, ConnectionSummary } from '../../api/types'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { FormField } from '../../components/FormField'
import { MutationAlert } from '../../components/MutationAlert'
import { Panel } from '../../components/Panel'
import { describeFailure, type MutationFailure } from '../../lib/mutation'
import { useBeforeUnload } from '../../lib/unload'

type SslMode = ConnectionInput['ssl_mode']
type Form = Omit<ConnectionInput, 'port'> & { port: string }
type TextField = 'name' | 'host' | 'database_name' | 'username'

const SSL_MODES: SslMode[] = ['disable', 'require', 'verify-full']
const FIELDS = ['name', 'host', 'port', 'database_name', 'username', 'password', 'ssl_mode']
const NAME_MAX = 100

const EMPTY: Form = { name: '', host: '', port: '5432', database_name: '', username: '', password: '', ssl_mode: 'require' }

function fromSummary(c: ConnectionSummary): Form {
  return { name: c.name, host: c.host, port: String(c.port), database_name: c.database_name, username: c.username, password: '', ssl_mode: c.ssl_mode }
}

function validate(form: Form, creating: boolean): Record<string, string> {
  const errors: Record<string, string> = {}
  const name = form.name.trim()
  if (name.length === 0 || [...name].length > NAME_MAX) errors.name = `Nama harus 1 sampai ${NAME_MAX} karakter.`
  if (form.host.trim() === '') errors.host = 'Host wajib diisi.'
  if (form.database_name.trim() === '') errors.database_name = 'Nama database wajib diisi.'
  if (form.username.trim() === '') errors.username = 'Username wajib diisi.'
  if (!/^\d+$/.test(form.port.trim()) || Number(form.port) < 1 || Number(form.port) > 65535) {
    errors.port = 'Port harus bilangan bulat 1 sampai 65535.'
  }
  if (creating && form.password === '') errors.password = 'Password wajib diisi.'
  return errors
}

type Props = {
  connection: ConnectionSummary | null
  onSaved: (connection: ConnectionSummary, created: boolean) => void
  onClose?: () => void
}

export function ConnectionForm({ connection, onSaved, onClose }: Props) {
  const creating = connection === null
  const initial = connection ? fromSummary(connection) : EMPTY
  const [form, setForm] = useState<Form>(initial)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [failure, setFailure] = useState<MutationFailure | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [pending, setPending] = useState(false)

  const changed = (Object.keys(form) as (keyof Form)[]).filter((k) => form[k] !== initial[k])
  useBeforeUnload(changed.length > 0)

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (pending) return
    setFailure(null)
    setNotice(null)
    const local = validate(form, creating)
    setErrors(local)
    if (Object.keys(local).length > 0) return

    const values: ConnectionInput = {
      name: form.name.trim(),
      host: form.host.trim(),
      port: Number(form.port),
      database_name: form.database_name.trim(),
      username: form.username.trim(),
      password: form.password,
      ssl_mode: form.ssl_mode,
    }

    let res
    if (connection) {
      const patch: ConnectionPatchInput = {}
      for (const k of changed) {
        if (k === 'password') continue
        if (k === 'port') patch.port = values.port
        else if (k === 'ssl_mode') patch.ssl_mode = values.ssl_mode
        else patch[k as TextField] = values[k as TextField]
      }
      if (form.password !== '') patch.password = form.password
      if (patch.port === connection.port) delete patch.port
      if (Object.keys(patch).length === 0) {
        setNotice('Tidak ada perubahan untuk disimpan.')
        return
      }
      setPending(true)
      res = await updateConnection(connection.id, patch)
    } else {
      setPending(true)
      res = await createConnection(values)
    }
    setPending(false)

    if (res.ok) {
      setForm(creating ? EMPTY : fromSummary(res.data))
      setNotice(creating ? `Connection ${res.data.name} dibuat.` : 'Perubahan tersimpan.')
      onSaved(res.data, creating)
      return
    }
    const described = describeFailure(res, creating ? 'Connection gagal dibuat' : 'Perubahan gagal disimpan')
    if (!described) return
    setErrors(described.fieldErrors)
    setFailure(described)
  }

  const text = (field: TextField, label: string, extra: { maxLength?: number; hint?: string } = {}) => (
    <FormField label={label} required error={errors[field]} hint={extra.hint}>
      {(control) => (
        <input
          {...control}
          className={`form-control${errors[field] ? ' is-invalid' : ''}`}
          type="text"
          autoComplete="off"
          spellCheck={false}
          maxLength={extra.maxLength}
          value={form[field]}
          disabled={pending}
          onChange={(e) => setForm({ ...form, [field]: e.target.value })}
        />
      )}
    </FormField>
  )

  return (
    <Panel
      title={connection ? <>Edit connection: {connection.name}</> : 'Tambah connection'}
      actions={
        onClose && (
          <Button variant="link" onClick={onClose} disabled={pending}>
            Tutup
          </Button>
        )
      }
    >
      <form method="post" noValidate onSubmit={handleSubmit}>
        <MutationAlert failure={failure} shownFields={FIELDS} />
        {notice && <Alert variant="info">{notice}</Alert>}
        {text('name', 'Nama tampilan', { maxLength: NAME_MAX })}
        {text('host', 'Host')}
        <FormField label="Port" required error={errors.port}>
          {(control) => (
            <input
              {...control}
              className={`form-control${errors.port ? ' is-invalid' : ''}`}
              type="number"
              inputMode="numeric"
              min={1}
              max={65535}
              step={1}
              value={form.port}
              disabled={pending}
              onChange={(e) => setForm({ ...form, port: e.target.value })}
            />
          )}
        </FormField>
        {text('database_name', 'Nama database')}
        {text('username', 'Username PostgreSQL')}
        <FormField
          label="Password PostgreSQL"
          required={creating}
          error={errors.password}
          hint={creating ? undefined : 'Password tersimpan tidak pernah ditampilkan. Kosong = password lama dipertahankan.'}
        >
          {(control) => (
            <input
              {...control}
              className={`form-control${errors.password ? ' is-invalid' : ''}`}
              type="password"
              autoComplete="new-password"
              placeholder={creating ? undefined : 'Biarkan kosong jika tidak diubah'}
              value={form.password}
              disabled={pending}
              onChange={(e) => setForm({ ...form, password: e.target.value })}
            />
          )}
        </FormField>
        <FormField label="SSL mode" required error={errors.ssl_mode}>
          {(control) => (
            <select
              {...control}
              className={`form-select${errors.ssl_mode ? ' is-invalid' : ''}`}
              value={form.ssl_mode}
              disabled={pending}
              onChange={(e) => setForm({ ...form, ssl_mode: e.target.value as SslMode })}
            >
              {SSL_MODES.map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
          )}
        </FormField>
        {!creating && (
          <p className="text-secondary">
            Mengubah host, port, atau nama database ditolak selama target sedang menjalankan migration.
          </p>
        )}
        <Button type="submit" pending={pending}>
          {creating ? 'Tambah connection' : 'Simpan perubahan'}
        </Button>
      </form>
    </Panel>
  )
}
