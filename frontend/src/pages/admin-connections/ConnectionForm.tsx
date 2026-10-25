import { useState, type FormEvent } from 'react'
import type { ApiResult } from '../../api/client'
import { createConnection, updateConnection } from '../../api/endpoints'
import type {
  ConnectionCredentialPatch,
  ConnectionInput,
  ConnectionPatchInput,
  ConnectionSummary,
} from '../../api/types'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { ConfirmDialog } from '../../components/ConfirmDialog'
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
const hasCredentials = (c: ConnectionSummary | null) => ROLES.every((r) => savedUsername(c, r) !== '')

const EXECUTOR_SUFFIX = '_executor'
const PREPARER_SUFFIX = '_preparer'

function sendsCredentials(form: Form, connection: ConnectionSummary | null): boolean {
  return hasCredentials(connection) || CREDENTIAL_FIELDS.some((f) => form[f].trim() !== '')
}

function validateCredentials(form: Form, connection: ConnectionSummary | null): Record<string, string> {
  const errors: Record<string, string> = {}
  if (!sendsCredentials(form, connection)) return errors
  if (hasCredentials(connection)) {
    for (const role of ROLES) {
      if (form[`${role}_username`].trim() === '') {
        errors[`${role}_username`] = 'Untuk menghapus kredensial, gunakan tombol Hapus kredensial eksekusi.'
      }
    }
  } else {
    for (const f of CREDENTIAL_FIELDS) {
      if (form[f].trim() === '') errors[f] = 'Isi keempat kredensial eksekusi atau kosongkan semuanya.'
    }
  }
  return { ...validateRoleNames(form), ...errors }
}

function validateRoleNames(form: Form): Record<string, string> {
  const errors: Record<string, string> = {}
  const executor = form.executor_username.trim()
  const preparer = form.preparer_username.trim()
  const viewer = form.username.trim()
  const prefix = executor.endsWith(EXECUTOR_SUFFIX) ? executor.slice(0, -EXECUTOR_SUFFIX.length) : ''
  if (executor !== '') {
    if (prefix === '') errors.executor_username = `Username executor harus berakhiran ${EXECUTOR_SUFFIX} (mis. app${EXECUTOR_SUFFIX}).`
    else if (executor === viewer) errors.executor_username = 'Username executor harus berbeda dari username PostgreSQL (viewer).'
  }
  if (preparer !== '' && executor !== '') {
    if (prefix !== '' && preparer !== prefix + PREPARER_SUFFIX) {
      errors.preparer_username = `Username preparer harus ${prefix}${PREPARER_SUFFIX}.`
    } else if (preparer === viewer) {
      errors.preparer_username = 'Username preparer harus berbeda dari username PostgreSQL (viewer).'
    }
  }
  return errors
}

function credential(form: Form, role: Role): ConnectionCredentialPatch {
  const username = form[`${role}_username`].trim()
  const password = form[`${role}_password`]
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
  const [confirmClear, setConfirmClear] = useState(false)

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
    const withCredentials = sendsCredentials(form, connection)

    if (connection) {
      if (changed.length === 0) {
        setNotice('Tidak ada perubahan untuk disimpan.')
        return
      }
      const { password, ...rest } = values
      const patch: ConnectionPatchInput = password === '' ? rest : { ...rest, password }
      if (withCredentials) {
        patch.executor = credential(form, 'executor')
        patch.preparer = credential(form, 'preparer')
      }
      await save(updateConnection(connection.id, patch), 'Perubahan tersimpan.')
    } else {
      if (withCredentials) {
        values.executor = { username: form.executor_username.trim(), password: form.executor_password }
        values.preparer = { username: form.preparer_username.trim(), password: form.preparer_password }
      }
      await save(createConnection(values), null)
    }
  }

  async function clearCredentials() {
    if (!connection || pending) return
    setFailure(null)
    setNotice(null)
    setErrors({})
    const patch: ConnectionPatchInput = {
      name: connection.name,
      host: connection.host,
      port: connection.port,
      database_name: connection.database_name,
      username: connection.username,
      ssl_mode: connection.ssl_mode,
    }
    await save(updateConnection(connection.id, patch), 'Kredensial eksekusi dihapus. Connection ini sekarang view-only.')
    setConfirmClear(false)
  }

  async function save(request: Promise<ApiResult<ConnectionSummary>>, savedNotice: string | null) {
    setPending(true)
    const res = await request
    setPending(false)

    if (res.ok) {
      setForm(creating ? EMPTY : fromSummary(res.data))
      setNotice(savedNotice ?? `Connection ${res.data.name} dibuat.`)
      onSaved(res.data, creating)
      return
    }
    const described = describeFailure(res, creating ? 'Connection gagal dibuat' : 'Perubahan gagal disimpan')
    if (!described) return
    if (res.kind === 'http' && res.error.code === 'TARGET_UNAVAILABLE') described.text = UNVERIFIED
    if (res.kind === 'http' && res.error.code === 'CONFLICT') described.text += ' Muat ulang halaman untuk melihat data terbaru.'
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

  const configured = hasCredentials(connection)
  const credentialInputs = (role: Role) => {
    const usernameField = `${role}_username` as const
    const passwordField = `${role}_password` as const
    return (
      <div className="col-md-6" key={role}>
        <FormField
          label={`Username ${role}`}
          error={errors[usernameField]}
          hint={role === 'executor' ? `Berakhiran ${EXECUTOR_SUFFIX}.` : `Prefiks executor + ${PREPARER_SUFFIX}.`}
        >
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
            {configured
              ? 'Kredensial tersimpan tetap dipakai saat menyimpan perubahan lain.'
              : 'Isi keempatnya agar migration bisa dijalankan, atau kosongkan semuanya untuk connection view-only.'}{' '}
            Username executor wajib berakhiran <code>{EXECUTOR_SUFFIX}</code>, username preparer wajib berakhiran <code>{PREPARER_SUFFIX}</code> (mis. <code>wbd_target_a_executor</code> dan{' '}
            <code>wbd_target_a_preparer</code>), dan keduanya harus berbeda dari username PostgreSQL di atas.
          </p>
          <div className="row">{ROLES.map(credentialInputs)}</div>
          {configured && (
            <div className="d-flex flex-wrap align-items-center gap-2">
              <Button
                variant="outline-danger"
                disabled={pending || changed.length > 0}
                onClick={() => setConfirmClear(true)}
              >
                Hapus kredensial eksekusi (jadikan view-only)
              </Button>
              {changed.length > 0 && <span className="text-secondary">Simpan atau batalkan perubahan lain dulu.</span>}
            </div>
          )}
        </fieldset>
        {!creating && (
          <p className="text-secondary">
            Selama target sedang menjalankan migration, perubahan apa pun selain nama tampilan (host, port, database,
            username, password, SSL mode, atau kredensial eksekusi) ditolak.
          </p>
        )}
        <Button type="submit" pending={pending}>
          {creating ? 'Tambah connection' : 'Simpan perubahan'}
        </Button>
      </form>
      <ConfirmDialog
        open={confirmClear}
        title="Hapus kredensial eksekusi?"
        confirmLabel="Ya, jadikan view-only"
        danger
        pending={pending}
        onCancel={() => setConfirmClear(false)}
        onConfirm={() => void clearCredentials()}
      >
        <p className="mb-2">
          Kredensial executor dan preparer untuk <strong>{connection?.name}</strong> akan dihapus dari server.
        </p>
        <p className="mb-0">
          Schema dan data tetap bisa dilihat, tetapi migration tidak bisa dijalankan pada database ini sampai kredensial
          eksekusi diisi lagi.
        </p>
      </ConfirmDialog>
    </Panel>
  )
}
