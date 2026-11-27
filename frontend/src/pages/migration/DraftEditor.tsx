import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react'
import { Link } from 'react-router'
import { errorMessage, isErrorCode, type ApiFailure, type ApiResult } from '../../api/client'
import { getDraft } from '../../api/endpoints'
import type { DatabaseSummary, Draft, ID } from '../../api/types'
import { updateMigrationDraft } from '../../api/workflow'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { FailureAlert } from '../../components/FailureAlert'
import { LeaveGuard } from '../../components/LeaveGuard'
import { MigrationSpec } from '../../components/MigrationSpec'
import { Panel } from '../../components/Panel'
import { FormSkeleton } from '../../components/Skeleton'
import { WorkflowAlert } from '../../components/WorkflowAlert'
import { formatDateTime } from '../../lib/format'
import { paths } from '../../lib/paths'
import { EMPTY_FORM, formFromDraft, sameForm, toDraftInput, validateDraft, visibleFields, type DraftForm } from './draft'
import { DraftFields } from './DraftFields'
import { ProposePanel } from './ProposePanel'

type LoadState =
  | { status: 'loading' }
  | { status: 'ready'; saved: Draft }
  | { status: 'failed'; failure: ApiFailure }

type Notice = { variant: 'success' | 'warning' | 'danger'; title: string; text: string }

type Props = { database: DatabaseSummary; draftId: ID }

export function DraftEditor({ database, draftId }: Props) {
  const [load, setLoad] = useState<LoadState>({ status: 'loading' })
  const [form, setForm] = useState<DraftForm>(EMPTY_FORM)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [saving, setSaving] = useState(false)
  const [notice, setNotice] = useState<Notice | null>(null)
  const [saveFailure, setSaveFailure] = useState<ApiFailure | null>(null)
  const [conflict, setConflict] = useState(false)
  const [confirmReload, setConfirmReload] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const seq = useRef(0)

  const applyDraft = useCallback((mode: 'initial' | 'refresh', res: ApiResult<Draft>, next: Notice | null) => {
    setRefreshing(false)
    if (res.ok) {
      setLoad({ status: 'ready', saved: res.data })
      setForm(formFromDraft(res.data))
      setErrors({})
      setSaveFailure(null)
      setConflict(false)
      setNotice(next)
    } else if (mode === 'initial') {
      setLoad({ status: 'failed', failure: res })
    } else {
      setNotice({ variant: 'danger', title: 'Gagal memuat ulang draft', text: errorMessage(res) })
    }
  }, [])

  const requestDraft = useCallback(
    async (mode: 'initial' | 'refresh', next: Notice | null) => {
      const mine = ++seq.current
      const res = await getDraft(database.id, draftId)
      if (mine === seq.current) applyDraft(mode, res, next)
    },
    [database.id, draftId, applyDraft],
  )

  const fetchDraft = useCallback(
    (mode: 'initial' | 'refresh', next: Notice | null = null) => {
      if (mode === 'initial') setLoad({ status: 'loading' })
      else setRefreshing(true)
      return requestDraft(mode, next)
    },
    [requestDraft],
  )

  useEffect(() => {
    void requestDraft('initial', null)
    const ref = seq
    return () => {
      ref.current++
    }
  }, [requestDraft])

  const saved = load.status === 'ready' ? load.saved : null
  const dirty = saved !== null && !sameForm(form, formFromDraft(saved))

  async function handleSave(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!saved || saving || conflict) return
    setNotice(null)
    setSaveFailure(null)
    const local = validateDraft(form)
    setErrors(local)
    if (Object.keys(local).length > 0) return

    setSaving(true)
    const mine = ++seq.current
    const res = await updateMigrationDraft(database.id, draftId, { ...toDraftInput(form), revision: saved.revision })
    if (mine !== seq.current) return
    setSaving(false)
    if (res.ok) {
      setLoad({ status: 'ready', saved: res.data })
      setForm(formFromDraft(res.data))
      setNotice({ variant: 'success', title: 'Tersimpan', text: `Draft tersimpan sebagai revision ${res.data.revision}.` })
      return
    }
    if (isErrorCode(res, 'VERSION_CONFLICT')) {
      setConflict(true) 
      return
    }
    if (isErrorCode(res, 'ALREADY_APPLIED')) {
      void fetchDraft('refresh', { variant: 'warning', title: 'Draft sudah applied', text: errorMessage(res) })
      return
    }
    if (res.kind === 'http') setErrors(res.error.field_errors)
    setSaveFailure(res)
  }

  function requestReload() {
    if (dirty) setConfirmReload(true)
    else void fetchDraft('refresh')
  }

  if (load.status === 'loading') {
    return (
      <Panel title="Edit draft">
        <FormSkeleton label="Memuat draft…" fields={4} />
      </Panel>
    )
  }
  if (load.status === 'failed') {
    return (
      <FailureAlert
        failure={load.failure}
        notFoundTitle="Draft tidak ditemukan"
        invalidTitle="ID draft tidak valid"
        onRetry={() => void fetchDraft('initial')}
      />
    )
  }

  const draft = load.saved
  const applied = draft.applied_execution_id !== null

  return (
    <>
      <LeaveGuard when={dirty || saving} />
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
        {notice && (
          <Alert variant={notice.variant} title={notice.title}>
            {notice.text}
          </Alert>
        )}
        <WorkflowAlert failure={saveFailure} title="Draft gagal disimpan" shownFields={visibleFields(form)} />

        {applied ? (
          <>
            <Alert variant="info" title="Draft sudah applied">
              Draft ini sudah berhasil dijalankan dan tidak dapat diubah.{' '}
              {draft.applied_execution_id && (
                <Link to={paths.execution(database.id, draft.applied_execution_id)}>Lihat execution</Link>
              )}
            </Alert>
            <MigrationSpec
              spec={{
                title: draft.title,
                revision: draft.revision,
                sql_up: draft.sql,
                sql_down: draft.sql_down ?? null,
                irreversible_reason: draft.irreversible_reason ?? null,
                mode: draft.mode ?? 'standard',
                batch: draft.batch ?? null,
              }}
            />
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
          <Link to={paths.migrations(database.id)}>Kembali ke riwayat migration</Link>
        </p>
      </Panel>

      {!applied && (
        <ProposePanel
          database={database}
          draft={draft}
          onConflict={() => setConflict(true)}
          blockedReason={
            conflict
              ? 'Draft berubah di tempat lain; muat versi terbaru dulu.'
              : saving
                ? 'Sedang menyimpan.'
                : dirty
                  ? 'Simpan perubahan dulu.'
                  : null
          }
        />
      )}

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
