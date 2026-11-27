import type { ReactNode } from 'react'
import { Link } from 'react-router'
import type { HistoryAttemptRow, ID } from '../../api/types'
import { ShortId } from '../../components/ShortId'
import { ActionBadge, AttemptBadge } from '../../components/StateBadge'
import { formatDateTime } from '../../lib/format'
import { paths } from '../../lib/paths'
import { Statements } from './Statements'

const UNKNOWN = <span className="text-secondary fst-italic">tidak diketahui</span>

function sqlEntered(v: boolean | null): ReactNode {
  if (v === null) return UNKNOWN
  return v ? 'Ya' : 'SQL belum dijalankan'
}

type Props = { databaseId: ID; row: HistoryAttemptRow; actions?: ReactNode }

export function Attempt({ databaseId, row: a, actions }: Props) {
  const c = a.commit
  return (
    <li className={`history-item card${a.is_active_head ? ' history-item--head' : ''}`}>
      <div className="card-body">
        <div className="d-flex flex-wrap align-items-center gap-2 mb-2">
          <ActionBadge action={a.action} direction={a.direction} />
          <AttemptBadge status={a.status} />
          <span className="mono fw-bold">{a.version === null ? '-' : `v${a.version}`}</span>
          {a.is_active_head && <span className="badge bg-green text-green-fg">Posisi aktif</span>}
        </div>

        <dl className="history-meta">
          <div>
            <dt>Actor</dt>
            <dd>{a.actor ? a.actor.username : UNKNOWN}</dd>
          </div>
          <div>
            <dt>Mulai</dt>
            <dd>{formatDateTime(a.started_at)}</dd>
          </div>
          <div>
            <dt>Selesai</dt>
            <dd>{a.finished_at === null ? 'belum selesai' : formatDateTime(a.finished_at)}</dd>
          </div>
          <div>
            <dt>SQL dijalankan</dt>
            <dd>{sqlEntered(a.sql_entered)}</dd>
          </div>
          <div>
            <dt>Attempt</dt>
            <dd>
              <ShortId id={a.attempt_id} />
            </dd>
          </div>
          <div>
            <dt>Execution</dt>
            <dd>
              <ShortId id={a.execution_id} />
            </dd>
          </div>
          <div>
            <dt>Revision</dt>
            <dd>{a.revision ?? UNKNOWN}</dd>
          </div>
          {a.source_execution_id && (
            <div>
              <dt>Sumber</dt>
              <dd>
                <ShortId id={a.source_execution_id} />
              </dd>
            </div>
          )}
        </dl>

        {c && (
          <div className="history-commit">
            <span className="badge bg-green-lt">Commit</span>
            <span className="small">{formatDateTime(c.committed_at)}</span>
            <span className="small">{c.is_complete ? 'lengkap' : 'belum lengkap'}</span>
            {c.batch_commits.length > 0 && <span className="small">{c.batch_commits.length} batch</span>}
            <Link className="ms-auto" to={paths.execution(databaseId, c.execution_id)}>
              Detail commit
            </Link>
          </div>
        )}

        <Statements statements={a.statements} sqlEntered={a.sql_entered} />

        {actions && <div className="history-item__actions">{actions}</div>}
      </div>
    </li>
  )
}
