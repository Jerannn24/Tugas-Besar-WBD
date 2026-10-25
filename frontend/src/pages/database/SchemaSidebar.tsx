import type { SchemaSummary } from '../../api/types'
import { EmptyState } from '../../components/EmptyState'

export type Selection = { schema: string; table: string }

type Props = {
  schemas: SchemaSummary[]
  selected: Selection | null
  onSelect: (selection: Selection) => void
}

export function SchemaSidebar({ schemas, selected, onSelect }: Props) {
  if (schemas.length === 0) {
    return (
      <EmptyState title="Tidak ada schema">
        Database ini tidak memiliki schema yang dapat ditampilkan.
      </EmptyState>
    )
  }

  return (
    <nav aria-label="Schema dan table" className="viewer-sidebar">
      {schemas.map((s) => (
        <div key={s.name} className="mb-3">
          <h3 className="subheader mb-1">{s.name}</h3>
          {s.tables.length === 0 ? (
            <p className="text-secondary small fst-italic mb-0">Tidak ada table</p>
          ) : (
            <div className="list-group list-group-flush">
              {s.tables.map((t) => {
                const active = selected?.schema === s.name && selected.table === t.name
                return (
                  <button
                    key={t.name}
                    type="button"
                    className={`list-group-item list-group-item-action py-1${active ? ' active' : ''}`}
                    aria-current={active ? 'true' : undefined}
                    onClick={() => onSelect({ schema: s.name, table: t.name })}
                  >
                    {t.name}
                  </button>
                )
              })}
            </div>
          )}
        </div>
      ))}
    </nav>
  )
}
