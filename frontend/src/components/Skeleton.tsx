import type { ReactNode } from 'react'

const WIDTHS = ['col-8', 'col-6', 'col-10', 'col-5', 'col-7', 'col-9']

function Bar({ i, className = '' }: { i: number; className?: string }) {
  return <span className={`placeholder ${WIDTHS[i % WIDTHS.length]} ${className}`.trim()} />
}

function Busy({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="placeholder-glow" role="status" aria-busy="true" aria-live="polite">
      <span className="visually-hidden">{label}</span>
      <div aria-hidden="true">{children}</div>
    </div>
  )
}

type TableProps = { label: string; columns: number | string[]; rows?: number }

// DataTable
export function TableSkeleton({ label, columns, rows = 5 }: TableProps) {
  const headers = typeof columns === 'number' ? Array.from({ length: columns }, () => null) : columns
  return (
    <Busy label={label}>
      <div className="table-responsive">
        <table className="table table-vcenter table-bordered card-table mb-0 skeleton-table">
          <thead>
            <tr>
              {headers.map((h, j) => (
                <th key={j} scope="col">
                  {h ?? <Bar i={j} />}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {Array.from({ length: rows }, (_, i) => (
              <tr key={i}>
                {headers.map((_, j) => (
                  <td key={j}>
                    <Bar i={i + j} />
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Busy>
  )
}

// list-group (daftar schema/table, daftar group)
export function ListSkeleton({ label, items = 5, heading = false }: { label: string; items?: number; heading?: boolean }) {
  return (
    <Busy label={label}>
      {heading && <Bar i={3} className="mb-2 d-block" />}
      <div className="list-group list-group-flush">
        {Array.from({ length: items }, (_, i) => (
          <div key={i} className="list-group-item py-2">
            <Bar i={i} />
          </div>
        ))}
      </div>
    </Busy>
  )
}

// Form
export function FormSkeleton({ label, fields = 3 }: { label: string; fields?: number }) {
  return (
    <Busy label={label}>
      {Array.from({ length: fields }, (_, i) => (
        <div key={i} className="mb-3">
          <Bar i={i + 3} className="mb-2 d-block" />
          <span className="placeholder col-12 placeholder-lg d-block" />
        </div>
      ))}
      <span className="placeholder btn btn-primary disabled col-4" />
    </Busy>
  )
}
