import { useState, type FormEvent } from 'react'
import { createConnection, updateConnection } from '../../api/endpoints'
import type {
  ConnectionCredentialPatch,
  ConnectionInput,
  ConnectionPatchInput,
  ConnectionSummary,
} from '../../api/types'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { FormField } from '../../components/FormField'
import { MutationAlert } from '../../components/MutationAlert'
import { Panel } from '../../components/Panel'
import { describeFailure, type MutationFailure } from '../../lib/mutation'
import { useBeforeUnload } from '../../lib/unload'

type SslMode = ConnectionInput['ssl_mode']
type Role = 'executor' | 'preparer'
type Form = Omit<ConnectionInput, 'port' | Role> & {
  port: string
  executor_username: string
  executor_password: string
  preparer_username: string
  preparer_password: string
}
type TextField = 'name' | 'host' | 'database_name' | 'username'

const SSL_MODES: SslMode[] = ['disable', 'require', 'verify-full']
const ROLES: Role[] = ['executor', 'preparer']
const CREDENTIAL_FIELDS = ['executor_username', 'executor_password', 'preparer_username', 'preparer_password'] as const
const FIELDS = ['name', 'host', 'port', 'database_name', 'username', 'password', 'ssl_mode', ...CREDENTIAL_FIELDS]
const NAME_MAX = 100
const UNVERIFIED = 'Kredensial tidak dapat diverifikasi ke database target. Tidak ada yang disimpan.'

const EMPTY: Form = {
  name: '',
  host: '',
  port: '5432',
  database_name: '',
  username: '',
  password: '',
  ssl_mode: 'require',
  executor_username: '',
  executor_password: '',
  preparer_username: '',
  preparer_password: '',
}

function fromSummary(c: ConnectionSummary): Form {
  return {
    name: c.name,
    host: c.host,
    port: String(c.port),
    database_name: c.database_name,
    username: c.username,
    password: '',
    ssl_mode: c.ssl_mode,
    executor_username: c.executor_username ?? '',
    executor_password: '',
    preparer_username: c.preparer_username ?? '',
    preparer_password: '',
  }
}

const savedUsername = (c: ConnectionSummary | null, role: Role) => (c ? c[`${role}_username`] ?? '' : '')

function validateCredentials(form: Form, connection: ConnectionSummary | null): Record<string, string> {
  const errors: Record<string, string> = {}
  const filled = CREDENTIAL_FIELDS.filter((f) => form[f].trim() !== '')
  if (connection === null) {
    if (filled.length > 0 && filled.length < CREDENTIAL_FIELDS.length) {
      for (const f of CREDENTIAL_FIELDS) {
        if (form[f].trim() === '') errors[f] = 'Isi keempat kredensial eksekusi atau kosongkan semuanya.'
      }
    }
    return errors
  }
  if (filled.length === 0 && ROLES.every((r) => savedUsername(connection, r) === '')) return errors
  for (const role of ROLES) {
    const username = form[`${role}_username`].trim()
    if (username === '') {
      errors[`${role}_username`] =
        savedUsername(connection, role) !== ''
          ? 'Menghapus kredensial eksekusi belum didukung kontrak.'
          : 'Isi kredensial executor dan preparer bersama-sama.'
    } else if (savedUsername(connection, role) === '' && form[`${role}_password`] === '') {
      errors[`${role}_password`] = 'Password wajib diisi untuk kredensial baru.'
    }
  }
  return errors
}

function credentialPatch(form: Form, connection: ConnectionSummary, role: Role): ConnectionCredentialPatch | null {
  const username = form[`${role}_username`].trim()
  const password = form[`${role}_password`]
  if (username === savedUsername(connection, role) && password === '') return null
  return password === '' ? { username } : { username, password }
}

function normalizeFieldErrors(fieldErrors: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [key, message] of Object.entries(fieldErrors)) out[key.replace('.', '_')] = message
  return out
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
    const local = { ...validate(form, creating), ...validateCredentials(form, connection) }
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
      const { password, ...rest } = values
      const executor = credentialPatch(form, connection, 'executor')
      const preparer = credentialPatch(form, connection, 'preparer')
      const unchanged =
        password === '' &&
        executor === null &&
        preparer === null &&
        rest.name === connection.name &&
        rest.host === connection.host &&
        rest.port === connection.port &&
        rest.database_name === connection.database_name &&
        rest.username === connection.username &&
        rest.ssl_mode === connection.ssl_mode
      if (unchanged) {
        setNotice('Tidak ada perubahan untuk disimpan.')
        return
      }
      const patch: ConnectionPatchInput = password === '' ? rest : { ...rest, password }
      if (executor) patch.executor = executor
      if (preparer) patch.preparer = preparer
      setPending(true)
      res = await updateConnection(connection.id, patch)
    } else {
      if (form.executor_username.trim() !== '') {
        values.executor = { username: form.executor_username.trim(), password: form.executor_password }
        values.preparer = { username: form.preparer_username.trim(), password: form.preparer_password }
      }
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
    if (res.kind === 'http' && res.error.code === 'TARGET_UNAVAILABLE') described.text = UNVERIFIED
    described.fieldErrors = normalizeFieldErrors(described.fieldErrors)
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

  const credential = (role: Role) => {
    const usernameField = `${role}_username` as const
    const passwordField = `${role}_password` as const
    const configured = savedUsername(connection, role) !== ''
    return (
      <div className="col-md-6" key={role}>
        <FormField label={`Username ${role}`} error={errors[usernameField]}>
          {(control) => (
            <input
              {...control}
              className={`form-control${errors[usernameField] ? ' is-invalid' : ''}`}
              type="text"
              autoComplete="off"
              spellCheck={false}
              value={form[usernameField]}
              disabled={pending}
              onChange={(e) => setForm({ ...form, [usernameField]: e.target.value })}
            />
          )}
        </FormField>
        <FormField
          label={`Password ${role}`}
          error={errors[passwordField]}
          hint={configured ? 'Kosong = password lama dipertahankan.' : undefined}
        >
          {(control) => (
            <input
              {...control}
              className={`form-control${errors[passwordField] ? ' is-invalid' : ''}`}
              type="password"
              autoComplete="new-password"
              placeholder={configured ? 'Biarkan kosong jika tidak diubah' : undefined}
              value={form[passwordField]}
              disabled={pending}
              onChange={(e) => setForm({ ...form, [passwordField]: e.target.value })}
            />
          )}
        </FormField>
      </div>
    )
  }

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
        <fieldset className="mb-3">
          <legend className="h4 mb-1">Kredensial eksekusi migration (opsional)</legend>
          <p className="text-secondary mb-2">
            Isi keempatnya agar migration bisa dijalankan, atau kosongkan semuanya untuk connection view-only. Server
            memverifikasi kredensial ke database target sebelum menyimpan.
          </p>
          <div className="row">{ROLES.map(credential)}</div>
        </fieldset>
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
