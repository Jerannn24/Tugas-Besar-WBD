import type { MutationFailure } from '../lib/mutation'
import { Alert } from './Alert'

type Props = {
  failure: MutationFailure | null
  shownFields?: string[]
}

export function MutationAlert({ failure, shownFields = [] }: Props) {
  if (!failure) return null
  const others = Object.entries(failure.fieldErrors).filter(([field]) => !shownFields.includes(field))
  return (
    <Alert variant="danger" title={failure.title}>
      <p className="mb-0">{failure.text}</p>
      {others.length > 0 && (
        <ul className="mb-0 mt-2">
          {others.map(([field, message]) => (
            <li key={field}>
              <code>{field}</code>: {message}
            </li>
          ))}
        </ul>
      )}
    </Alert>
  )
}
