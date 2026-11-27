import type { Approval, ID } from '../../api/types'
import { Button } from '../../components/Button'
import { EmptyState } from '../../components/EmptyState'
import { ShortId } from '../../components/ShortId'
import { formatDateTime } from '../../lib/format'

type Props = {
  approvals: Approval[]
  meId: ID
  canApprove: boolean
  pendingGroup: ID | null
  onApprove: (groupId: ID) => void
}

export function Approvals({ approvals, meId, canApprove, pendingGroup, onApprove }: Props) {
  if (approvals.length === 0) {
    return <EmptyState title="Belum ada approval wajib">Group yang wajib menyetujui ditentukan setelah validasi lolos.</EmptyState>
  }

  const approved = approvals.filter((a) => a.status === 'approved').length
  const noLeader = approvals.filter((a) => a.status === 'pending' && a.leader_user_id === null).length
  const mine = approvals.filter((a) => a.status === 'pending' && a.leader_user_id === meId).length
  const busy = pendingGroup !== null

  return (
    <>
      <p className="mb-2">
        <strong>
          {approved} dari {approvals.length}
        </strong>{' '}
        group sudah menyetujui.
        {approved < approvals.length && ` Masih kurang ${approvals.length - approved}.`}
      </p>
      {noLeader > 0 && (
        <p className="text-warning mb-2">
          {noLeader} group belum punya leader; proposal tetap menunggu sampai admin menetapkan leader.
        </p>
      )}
      {canApprove && mine > 1 && (
        <p className="text-secondary mb-2">Satu persetujuan Anda mencakup semua {mine} group yang Anda pimpin.</p>
      )}
      <ul className="list-group">
        {approvals.map((a) => {
          const isMine = a.leader_user_id === meId
          const waitingMe = a.status === 'pending' && isMine
          return (
            <li key={a.group_id} className="list-group-item d-flex flex-wrap align-items-center gap-2">
              <div className="me-auto" style={{ minWidth: 0 }}>
                <div>
                  Group <ShortId id={a.group_id} />
                </div>
                <div className="text-secondary small">
                  Leader:{' '}
                  {a.leader_user_id === null ? (
                    <span className="text-warning">Belum ada leader</span>
                  ) : isMine ? (
                    'Anda'
                  ) : (
                    <ShortId id={a.leader_user_id} />
                  )}
                  {a.approved_at && <> · disetujui {formatDateTime(a.approved_at)}</>}
                </div>
              </div>
              {a.status === 'approved' ? (
                <span className="badge bg-green-lt">Disetujui</span>
              ) : waitingMe ? (
                <span className="badge bg-blue-lt">Menunggu Anda</span>
              ) : (
                <span className="badge bg-yellow-lt">Menunggu</span>
              )}
              {canApprove && waitingMe && (
                <Button
                  variant="success"
                  pending={pendingGroup === a.group_id}
                  disabled={busy}
                  onClick={() => onApprove(a.group_id)}
                >
                  Setujui
                </Button>
              )}
            </li>
          )
        })}
      </ul>
    </>
  )
}
