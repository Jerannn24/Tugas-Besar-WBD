import type { ReactNode } from 'react'

export type DataColumn<T> = {
  key: string
  header: ReactNode
  render: (row: T, index: number) => ReactNode
  wrap?: boolean
}

type Props<T> = {
  columns: DataColumn<T>[]
  rows: T[]
  rowKey: (row: T, index: number) => string
  caption?: string
}

export function DataTable<T>({ columns, rows, rowKey, caption }: Props<T>) {
  return (
    <div className="table-scroll">
      <table className="table table-vcenter table-bordered card-table mb-0">
        {caption && <caption className="visually-hidden">{caption}</caption>}
        <thead>
          <tr>
            {columns.map((c) => (
              <th key={c.key} scope="col">
                {c.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, i) => (
            <tr key={rowKey(row, i)}>
              {columns.map((c) => (
                <td key={c.key} className={c.wrap ? 'cell-wrap' : undefined}>
                  {c.render(row, i)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
