import type { HistoryStatement } from '../../api/types'
import { OutcomeBadge } from '../../components/StateBadge'

type Props = { statements: HistoryStatement[]; sqlEntered: boolean | null }

export function Statements({ statements, sqlEntered }: Props) {
  if (statements.length === 0) {
    return (
      <p className="text-secondary small mb-0">
        {sqlEntered === null ? 'Salinan SQL aktual tidak tersedia (data lama).' : 'Tidak ada statement.'}
      </p>
    )
  }
  return (
    <details className="history-details">
      <summary>Statement ({statements.length})</summary>
      <ol className="history-statements">
        {statements.map((s) => (
          <li key={`${s.batch_index ?? '-'}:${s.ordinal}`}>
            <div className="d-flex flex-wrap align-items-center gap-2 mb-1">
              <span className="text-secondary small">#{s.ordinal}</span>
              {s.batch_index !== null && <span className="text-secondary small">batch {s.batch_index}</span>}
              <OutcomeBadge outcome={s.outcome} />
            </div>
            <pre className="sql-block">{s.sql}</pre>
            {Object.keys(s.parameters).length > 0 && (
              <>
                <div className="text-secondary small mt-2 mb-1">Parameter</div>
                <pre className="sql-block params-block">{JSON.stringify(s.parameters, null, 2)}</pre>
              </>
            )}
          </li>
        ))}
      </ol>
    </details>
  )
}
