import { useEffect, useState } from 'react'
import type { ApiFailure } from '../../api/client'
import { getGroupMembers, setGroupLeader } from '../../api/endpoints'
import type { Group, ID, User } from '../../api/types'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { FailureAlert } from '../../components/FailureAlert'
import { Loading } from '../../components/Loading'
import { MutationAlert } from '../../components/MutationAlert'
import { Panel } from '../../components/Panel'
import { ShortId } from '../../components/ShortId'
import { describeFailure, type MutationFailure } from '../../lib/mutation'

type Props = {
  group: Group
  users: User[]
  membersVersion: number
  onSaved: (group: Group) => void
}

type LoadState = { status: 'loading' } | { status: 'ready'; memberIds: ID[] } | { status: 'failed'; failure: ApiFailure }

export function LeaderEditor({ group, users, membersVersion, onSaved }: Props) {
  const [load, setLoad] = useState<LoadState>({ status: 'loading' })
  const [attempt, setAttempt] = useState(0)
  const [choice, setChoice] = useState<ID>('')
  const [pending, setPending] = useState(false)
  const [failure, setFailure] = useState<MutationFailure | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  useEffect(() => {
    let active = true
    getGroupMembers(group.id).then((res) => {
      if (!active) return
      setLoad(res.ok ? { status: 'ready', memberIds: res.data.user_ids } : { status: 'failed', failure: res })
    })
    return () => {
      active = false
    }
  }, [group.id, membersVersion, attempt])

  const leader = group.leader_user_id
  const leaderUser = users.find((u) => u.id === leader) ?? null
  const candidates =
    load.status === 'ready' ? users.filter((u) => u.is_active && load.memberIds.includes(u.id)) : []
  const selected = candidates.some((u) => u.id === choice) ? choice : ''

  async function save() {
    if (pending || selected === '') return
    setPending(true)
    setFailure(null)
    setNotice(null)
    const res = await setGroupLeader(group.id, { user_id: selected })
    setPending(false)
    if (res.ok) {
      onSaved(res.data)
      setChoice('')
      const name = users.find((u) => u.id === res.data.leader_user_id)?.username ?? 'user terpilih'
      setNotice(`Leader group sekarang ${name}.`)
      return
    }
    setFailure(describeFailure(res, 'Leader gagal ditetapkan'))
    
    if (res.kind === 'http' && res.status === 409) setAttempt((n) => n + 1)
  }

  return (
    <Panel title="Leader group">
      <p className="text-secondary">
        Leader menyetujui proposal migration untuk database yang menjadi grant group ini. Group tanpa leader membuat
        proposal tetap menunggu approval.
      </p>
      <dl className="mb-3">
        <dt>Leader saat ini</dt>
        <dd className="mb-0">
          {leader === null ? (
            <span className="text-warning">Belum ada leader</span>
          ) : leaderUser ? (
            <span className="mono">{leaderUser.username}</span>
          ) : (
            <ShortId id={leader} />
          )}
        </dd>
      </dl>
      {load.status === 'loading' && <Loading label="Memuat anggota…" />}
      {load.status === 'failed' && (
        <FailureAlert
          failure={load.failure}
          notFoundTitle="Group tidak ditemukan"
          onRetry={() => {
            setLoad({ status: 'loading' })
            setAttempt((n) => n + 1)
          }}
        />
      )}
      {load.status === 'ready' && (
        <>
          <MutationAlert failure={failure} />
          {notice && <Alert variant="success">{notice}</Alert>}
          {candidates.length === 0 ? (
            <p className="text-secondary mb-0">Tambahkan anggota aktif ke group ini sebelum memilih leader.</p>
          ) : (
            <div className="leader-form">
              <label className="form-label" htmlFor={`leader-${group.id}`}>
                Pilih leader dari anggota aktif
              </label>
              <select
                id={`leader-${group.id}`}
                className="form-select"
                value={selected}
                disabled={pending}
                onChange={(e) => {
                  setNotice(null)
                  setChoice(e.target.value)
                }}
              >
                <option value="">— pilih anggota —</option>
                {candidates.map((u) => (
                  <option key={u.id} value={u.id}>
                    {u.username}
                    {u.id === leader ? ' (leader saat ini)' : ''}
                  </option>
                ))}
              </select>
              <Button onClick={() => void save()} pending={pending} disabled={selected === '' || selected === leader}>
                Tetapkan leader
              </Button>
            </div>
          )}
        </>
      )}
    </Panel>
  )
}
