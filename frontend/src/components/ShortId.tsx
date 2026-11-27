import { shortId } from '../lib/format'

export function ShortId({ id }: { id: string }) {
  return (
    <code className="mono" title={id}>
      {shortId(id)}
    </code>
  )
}
