import { useEffect, useState } from 'react'
import type { ApiFailure } from '../../api/client'
import { getGroupMembers, putGroupMembers } from '../../api/endpoints'
import type { Group, ID, User } from '../../api/types'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { FailureAlert } from '../../components/FailureAlert'
import { Loading } from '../../components/Loading'
import { MutationAlert } from '../../components/MutationAlert'
import { Panel } from '../../components/Panel'
import { describeFailure, type MutationFailure } from '../../lib/mutation'

type Props = {
  group: Group
  users: User[]
  onDirtyChange: (dirty: boolean) => void
}

type LoadState = { status: 'loading' } | { status: 'ready' } | { status: 'failed'; failure: ApiFailure }

const sameSet = (a: ID[], b: ID[]) => a.length === b.length && a.every((id) => b.includes(id))

export function MemberEditor({ group, users, onDirtyChange }: Props) {
  const [load, setLoad] = useState<LoadState>({ status: 'loading' })
  const [saved, setSaved] = useState<ID[]>([])
  const [selected, setSelected] = useState<ID[]>([])
  const [failure, setFailure] = useState<MutationFailure | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [pending, setPending] = useState(false)
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    let active = true
    getGroupMembers(group.id).then((res) => {
      if (!active) return
      if (!res.ok) {
        setLoad({ status: 'failed', failure: res })
        return
      }
      setSaved(res.data.user_ids)
      setSelected(res.data.user_ids)
      setLoad({ status: 'ready' })
    })
    return () => {
      active = false
    }
  }, [group.id, attempt])

  const dirty = load.status === 'ready' && !sameSet(saved, selected)
  useEffect(() => onDirtyChange(dirty), [dirty, onDirtyChange])

  // ID anggota yang tidak ada di daftar user tetap ditampilkan agar tidak hilang diam-diam.
  const unknownIds = [...saved, ...selected].filter((id, i, all) => all.indexOf(id) === i && !users.some((u) => u.id === id))

  function toggle(id: ID, checked: boolean) {
    setNotice(null)
    setSelected((s) => (checked ? [...s, id] : s.filter((x) => x !== id)))
  }

  async function save() {
    if (pending) return
    setPending(true)
    setFailure(null)
    setNotice(null)
    const res = await putGroupMembers(group.id, { user_ids: selected })
    setPending(false)
    if (res.ok) {
      setSaved(res.data.user_ids)
      setSelected(res.data.user_ids)
      setNotice(res.data.user_ids.length === 0 ? 'Semua anggota dihapus dari group.' : 'Anggota group tersimpan.')
      return
    }
    setFailure(describeFailure(res, 'Anggota gagal disimpan'))
  }

  return (
    <Panel title="Anggota group">
      {load.status === 'loading' && <Loading label="Memuat anggota…" />}
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
          {users.length === 0 && unknownIds.length === 0 ? (
            <p className="text-secondary">Belum ada user.</p>
          ) : (
            <div className="list-group mb-3">
              {users.map((u) => (
                <label key={u.id} className="list-group-item d-flex align-items-center gap-2">
                  <input
                    className="form-check-input m-0"
                    type="checkbox"
                    checked={selected.includes(u.id)}
                    disabled={pending}
                    onChange={(e) => toggle(u.id, e.target.checked)}
                  />
                  <span className="mono">{u.username}</span>
                  {u.is_admin && <span className="badge bg-blue-lt">Platform Admin</span>}
                  {!u.is_active && <span className="badge bg-red-lt">Nonaktif</span>}
                </label>
              ))}
              {unknownIds.map((id) => (
                <label key={id} className="list-group-item d-flex align-items-center gap-2">
                  <input
                    className="form-check-input m-0"
                    type="checkbox"
                    checked={selected.includes(id)}
                    disabled={pending}
                    onChange={(e) => toggle(id, e.target.checked)}
                  />
                  <span className="badge bg-yellow-lt">ID tidak dikenal</span>
                  <span className="mono">{id}</span>
                </label>
              ))}
            </div>
          )}
          <div className="d-flex gap-2 align-items-center">
            <Button onClick={() => void save()} pending={pending} disabled={!dirty}>
              Simpan anggota
            </Button>
          </div>
        </>
      )}
    </Panel>
  )
}
