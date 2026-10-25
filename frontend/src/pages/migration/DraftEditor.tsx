import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react'
import { errorMessage, isErrorCode, type ApiFailure } from '../../api/client'
import { getDraft, updateDraft } from '../../api/endpoints'
import type { DatabaseSummary, Draft, Execution, ID } from '../../api/types'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { FailureAlert } from '../../components/FailureAlert'
import { Loading } from '../../components/Loading'
import { Panel } from '../../components/Panel'
import { formatDateTime } from '../../lib/format'
import { pageUrl } from '../../lib/query'
import { useBeforeUnload } from '../../lib/unload'
import { DraftFields, validateDraft, type DraftForm } from './DraftFields'
import { ExecutePanel } from './ExecutePanel'
import { useExecute } from './execute'

type LoadState =
  | { status: 'loading' }
  | { status: 'ready'; saved: Draft }
  | { status: 'failed'; failure: ApiFailure }

type Props = { database: DatabaseSummary; draftId: ID }

export function DraftEditor({ database, draftId }: Props) {
  const [load, setLoad] = useState<LoadState>({ status: 'loading' })
  const [form, setForm] = useState<DraftForm>({ title: '', sql: '' })
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [saving, setSaving] = useState(false)
  const [saveMessage, setSaveMessage] = useState<{ variant: 'success' | 'danger'; title: string; text: string } | null>(
    null,
  )
  const [conflict, setConflict] = useState(false)
  const [confirmReload, setConfirmReload] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const seq = useRef(0)

  const fetchDraft = useCallback(
    async (mode: 'initial' | 'refresh') => {
      const mine = ++seq.current
      if (mode === 'initial') setLoad({ status: 'loading' })
      else setRefreshing(true)
      const res = await getDraft(database.id, draftId)
      if (mine !== seq.current) return
      setRefreshing(false)
      if (res.ok) {
        setLoad({ status: 'ready', saved: res.data })
        setForm({ title: res.data.title, sql: res.data.sql })
        setErrors({})
        setConflict(false)
        setSaveMessage(null)
      } else if (mode === 'initial') {
        setLoad({ status: 'failed', failure: res })
      } else {
        setSaveMessage({ variant: 'danger', title: 'Gagal memuat ulang draft', text: errorMessage(res) })
      }
    },
    [database.id, draftId],
  )

  useEffect(() => {
    void fetchDraft('initial')
  }, [fetchDraft])

  const onFinal = useCallback(
    (execution: Execution) => {
      // Sukses → draft menjadi applied; ambil ulang supaya editor menjadi read-only.
      if (execution.status === 'succeeded') void fetchDraft('refresh')
    },
    [fetchDraft],
  )
  const onRejected = useCallback(
    (failure: ApiFailure) => {
      if (isErrorCode(failure, 'VERSION_CONFLICT')) setConflict(true)
      if (isErrorCode(failure, 'ALREADY_APPLIED')) void fetchDraft('refresh')
    },
    [fetchDraft],
  )
  const exec = useExecute({ databaseId: database.id, draftId, onFinal, onRejected })

  const saved = load.status === 'ready' ? load.saved : null
  const dirty = saved !== null && (form.title !== saved.title || form.sql !== saved.sql)
  useBeforeUnload(dirty || exec.state.phase === 'posting')

  async function handleSave(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!saved || saving || conflict) return
    setSaveMessage(null)
    const local = validateDraft(form)
    setErrors(local)
    if (Object.keys(local).length > 0) return

    setSaving(true)
    const res = await updateDraft(database.id, draftId, {
      title: form.title.trim(),
      sql: form.sql,
      revision: saved.revision,
    })
    setSaving(false)
    if (res.ok) {
      setLoad({ status: 'ready', saved: res.data })
      setForm({ title: res.data.title, sql: res.data.sql })
      setSaveMessage({ variant: 'success', title: 'Tersimpan', text: `Draft tersimpan sebagai revision ${res.data.revision}.` })
      return
    }
    if (res.kind === 'http' && res.status === 401) return
    if (isErrorCode(res, 'VERSION_CONFLICT')) {
      setConflict(true) // teks lokal ga ditimpa
      return
    }
    if (isErrorCode(res, 'ALREADY_APPLIED')) void fetchDraft('refresh')
    if (res.kind === 'http' && res.error.code === 'VALIDATION_FAILED') setErrors(res.error.field_errors)
    setSaveMessage({
      variant: 'danger',
      title: res.kind === 'http' && res.status === 403 ? 'Akses ditolak' : 'Draft gagal disimpan',
      text: errorMessage(res),
    })
  }

  function requestReload() {
    if (dirty) setConfirmReload(true)
    else void fetchDraft('refresh')
  }

  if (load.status === 'loading') return <Loading label="Memuat draft…" />
  if (load.status === 'failed') {
    return (
      <FailureAlert
        failure={load.failure}
        notFoundTitle="Draft tidak ditemukan"
        onRetry={() => void fetchDraft('initial')}
      />
    )
  }

  const draft = load.saved
  const applied = draft.applied_execution_id !== null
  const historyUrl = pageUrl('/migrations/', { database: database.id })

  return (
    <>
      <Panel
        title={applied ? 'Draft (applied, read-only)' : 'Edit draft'}
        actions={
          <span className="d-flex flex-wrap align-items-center gap-2">
            <span className="badge bg-secondary-lt">Revision {draft.revision}</span>
            {dirty && <span className="badge bg-yellow-lt">Belum disimpan</span>}
          </span>
        }
      >
        <p className="text-secondary small">Terakhir diubah {formatDateTime(draft.updated_at)}</p>

        {conflict && (
          <Alert variant="warning" title="Draft berubah di tempat lain">
            <p className="mb-2">
              Draft sudah diubah sejak Anda membukanya, jadi perubahan Anda tidak disimpan dan tidak menimpa versi
              tersebut. Salin teks Anda bila perlu, lalu muat versi terbaru.
            </p>
            <Button variant="outline-secondary" pending={refreshing} onClick={requestReload}>
              Muat versi terbaru
            </Button>
          </Alert>
        )}
        {saveMessage && (
          <Alert variant={saveMessage.variant} title={saveMessage.title}>
            {saveMessage.text}
          </Alert>
        )}

        {applied ? (
          <>
            <Alert variant="info" title="Draft sudah applied">
              Draft ini sudah berhasil dijalankan dan tidak dapat diubah.{' '}
              {draft.applied_execution_id && (
                <a href={pageUrl('/migration/', { database: database.id, execution: draft.applied_execution_id })}>
                  Lihat execution
                </a>
              )}
            </Alert>
            <dl className="detail-list">
              <dt>Title</dt>
              <dd>{draft.title}</dd>
            </dl>
            <pre className="sql-block">{draft.sql}</pre>
          </>
        ) : (
          <form method="post" noValidate onSubmit={handleSave}>
            <DraftFields value={form} onChange={setForm} errors={errors} disabled={saving} />
            <div className="d-flex flex-wrap gap-2 align-items-center">
              <Button type="submit" pending={saving} disabled={!dirty || conflict}>
                Simpan
              </Button>
              {dirty && !conflict && (
                <Button variant="outline-secondary" disabled={saving} onClick={requestReload}>
                  Batalkan perubahan
                </Button>
              )}
            </div>
          </form>
        )}
        <p className="mt-3 mb-0">
          <a href={historyUrl}>Kembali ke riwayat migration</a>
        </p>
      </Panel>

      <ExecutePanel
        database={database}
        draft={draft}
        exec={exec}
        blockedReason={
          conflict
            ? 'Draft berubah di tempat lain; muat versi terbaru dulu.'
            : saving
              ? 'Sedang menyimpan.'
              : dirty
                ? 'Simpan perubahan dulu; Execute selalu memakai revision tersimpan.'
                : null
        }
      />

      <ConfirmDialog
        open={confirmReload}
        title="Buang perubahan lokal?"
        confirmLabel="Muat versi tersimpan"
        danger
        onCancel={() => setConfirmReload(false)}
        onConfirm={() => {
          setConfirmReload(false)
          void fetchDraft('refresh')
        }}
      >
        Perubahan yang belum disimpan akan hilang dan diganti dengan versi draft terbaru dari server.
      </ConfirmDialog>
    </>
  )
}
