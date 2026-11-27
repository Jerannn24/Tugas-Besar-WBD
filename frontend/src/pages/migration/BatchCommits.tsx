import { Link } from 'react-router'
import type { BatchCommit, ID } from '../../api/types'
import { ShortId } from '../../components/ShortId'
import { formatDateTime } from '../../lib/format'
import { paths } from '../../lib/paths'

function range(b: BatchCommit): string {
  if (b.range_start === null && b.range_end === null) return '-'
  return `${b.range_start ?? '?'} – ${b.range_end ?? '?'}`
}

export function BatchCommits({ databaseId, batches }: { databaseId: ID; batches: BatchCommit[] }) {
  return (
    <>
      <h3 className="h4">Batch commit ({batches.length})</h3>
      <ol className="history-list">
        {batches.map((b) => (
          <li key={`${b.batch_index}:${b.receipt}`} className="history-item card">
            <div className="card-body">
              <dl className="history-meta">
                <div>
                  <dt>Batch</dt>
                  <dd className="mono">{b.batch_index}</dd>
                </div>
                <div>
                  <dt>Baris</dt>
                  <dd className="mono">{b.row_count}</dd>
                </div>
                <div>
                  <dt>Range</dt>
                  <dd className="mono">{range(b)}</dd>
                </div>
                <div>
                  <dt>Commit</dt>
                  <dd>{formatDateTime(b.committed_at)}</dd>
                </div>
                <div>
                  <dt>Dijalankan oleh</dt>
                  <dd>{b.executed_by.username}</dd>
                </div>
                <div>
                  <dt>Job</dt>
                  <dd>
                    <Link to={paths.job(databaseId, b.job_id)}>
                      <ShortId id={b.job_id} />
                    </Link>
                  </dd>
                </div>
                <div>
                  <dt>Receipt</dt>
                  <dd className="mono">{b.receipt}</dd>
                </div>
              </dl>
              <details className="history-details">
                <summary>SQL dan parameter</summary>
                <pre className="sql-block">{b.sql_snapshot}</pre>
                <div className="text-secondary small mt-2 mb-1">Parameter aman</div>
                <pre className="sql-block params-block">{JSON.stringify(b.safe_parameters, null, 2)}</pre>
              </details>
            </div>
          </li>
        ))}
      </ol>
    </>
  )
}
