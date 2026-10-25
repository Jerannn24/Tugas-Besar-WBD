import { useEffect, useState } from 'react'
import type { ApiFailure } from '../../api/client'
import { getGroupGrants, putGroupGrants } from '../../api/endpoints'
import type { ConnectionSummary, Group, GroupGrants, ID } from '../../api/types'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { FailureAlert } from '../../components/FailureAlert'
import { Loading } from '../../components/Loading'
import { MutationAlert } from '../../components/MutationAlert'
import { Panel } from '../../components/Panel'
import { describeFailure, type MutationFailure } from '../../lib/mutation'

type Access = GroupGrants['grants'][number]['access']
type Choice = Access | 'none'
type Props = {
  group: Group
  connections: ConnectionSummary[]
  onDirtyChange: (dirty: boolean) => void
}

type LoadState = { status: 'loading' } | { status: 'ready' } | { status: 'failed'; failure: ApiFailure }

const CHOICES: { value: Choice; label: string }[] = [
  { value: 'none', label: 'Tidak ada' },
  { value: 'view', label: 'View Only' },
  { value: 'edit', label: 'View + Edit' },
]

function toMap(grants: GroupGrants['grants']): Record<ID, Access> {
  return Object.fromEntries(grants.map((g) => [g.database_id, g.access]))
}

function sameMap(a: Record<ID, Access>, b: Record<ID, Access>): boolean {
  const keys = Object.keys(a)
  return keys.length === Object.keys(b).length && keys.every((k) => a[k] === b[k])
}

export function GrantEditor({ group, connections, onDirtyChange }: Props) {
  const [load, setLoad] = useState<LoadState>({ status: 'loading' })
  const [saved, setSaved] = useState<Record<ID, Access>>({})
  const [draft, setDraft] = useState<Record<ID, Access>>({})
  const [failure, setFailure] = useState<MutationFailure | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [pending, setPending] = useState(false)
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    let active = true
    getGroupGrants(group.id).then((res) => {
      if (!active) return
      if (!res.ok) {
        setLoad({ status: 'failed', failure: res })
        return
      }
      setSaved(toMap(res.data.grants))
      setDraft(toMap(res.data.grants))
      setLoad({ status: 'ready' })
    })
    return () => {
      active = false
    }
  }, [group.id, attempt])

  const dirty = load.status === 'ready' && !sameMap(saved, draft)
  useEffect(() => onDirtyChange(dirty), [dirty, onDirtyChange])

  const rows: { id: ID; connection: ConnectionSummary | null }[] = [
    ...connections.map((c) => ({ id: c.id, connection: c })),
    ...Object.keys({ ...saved, ...draft })
      .filter((id) => !connections.some((c) => c.id === id))
      .map((id) => ({ id, connection: null })),
  ]

  function choose(id: ID, choice: Choice) {
    setNotice(null)
    setDraft((d) => {
      const next = { ...d }
      if (choice === 'none') delete next[id]
      else next[id] = choice
      return next
    })
  }

  async function save() {
    if (pending) return
    setPending(true)
    setFailure(null)
    setNotice(null)
    const grants = Object.entries(draft).map(([database_id, access]) => ({ database_id, access }))
    const res = await putGroupGrants(group.id, { grants })
    setPending(false)
    if (res.ok) {
      setSaved(toMap(res.data.grants))
      setDraft(toMap(res.data.grants))
      setNotice(res.data.grants.length === 0 ? 'Semua grant dicabut dari group.' : 'Grant group tersimpan.')
      return
    }
    setFailure(describeFailure(res, 'Grant gagal disimpan'))
  }

  return (
    <Panel title="Grant database">
      {load.status === 'loading' && <Loading label="Memuat grant…" />}
      {load.status === 'failed' && (
        <FailureAlert failure={load.failure} notFoundTitle="Group tidak ditemukan" onRetry={() => {
            setLoad({ status: 'loading' })
            setAttempt((n) => n + 1)
          }} />
      )}
      {load.status === 'ready' && (
        <>
          <MutationAlert failure={failure} />
          {notice && <Alert variant="success">{notice}</Alert>}
          <p className="text-secondary">
            Jika user ada di beberapa group, level tertinggi yang berlaku. 
          </p>
          {rows.length === 0 ? (
            <p className="text-secondary">
              Belum ada connection. Tambahkan di <a href="/admin/connections/">Admin: Connections</a>.
            </p>
          ) : (
            <div className="list-group mb-3">
              {rows.map(({ id, connection }) => (
                <div key={id} className="list-group-item d-flex flex-wrap align-items-center gap-2">
                  <div className="me-auto">
                    {connection ? (
                      <>
                        <strong>{connection.name}</strong>
                        <div className="text-secondary mono">
                          {connection.host}:{connection.port}/{connection.database_name}
                        </div>
                      </>
                    ) : (
                      <>
                        <span className="badge bg-yellow-lt me-2">Database tidak dikenal</span>
                        <span className="mono">{id}</span>
                      </>
                    )}
                  </div>
                  <select
                    className="form-select w-auto"
                    aria-label={`Level akses untuk ${connection?.name ?? id}`}
                    value={draft[id] ?? 'none'}
                    disabled={pending}
                    onChange={(e) => choose(id, e.target.value as Choice)}
                  >
                    {CHOICES.map((c) => (
                      <option key={c.value} value={c.value}>
                        {c.label}
                      </option>
                    ))}
                  </select>
                </div>
              ))}
            </div>
          )}
          <div className="d-flex gap-2 align-items-center">
            <Button onClick={() => void save()} pending={pending} disabled={!dirty}>
              Simpan grant
            </Button>
          </div>
        </>
      )}
    </Panel>
  )
}
