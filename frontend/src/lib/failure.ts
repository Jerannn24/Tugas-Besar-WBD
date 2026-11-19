import type { ApiFailure } from '../api/client'

export type FailureKind = 'expired' | 'invalid' | 'forbidden' | 'not_found' | 'unavailable' | 'error'

export function classifyFailure(failure: ApiFailure): FailureKind {
  if (failure.kind !== 'http') return 'error'
  if (failure.status === 400) return 'invalid'
  if (failure.status === 401) return 'expired'
  if (failure.status === 403) return 'forbidden'
  if (failure.status === 404) return 'not_found'
  if (failure.status === 502 || failure.error.code === 'TARGET_UNAVAILABLE') return 'unavailable'
  return 'error'
}

export type WorkflowFailureKind =
  | 'network'
  | 'invalid_response'
  | 'invalid_request'
  | 'session_ended'
  | 'csrf'
  | 'forbidden'
  | 'not_found'
  | 'conflict'
  | 'validation'
  | 'not_implemented'
  | 'target_unavailable'
  | 'queue_unavailable'
  | 'server'

export type WorkflowFailure = {
  kind: WorkflowFailureKind
  code: string | null
  fieldErrors: Record<string, string>
  reload: boolean
  retrySameRequest: boolean
}

const CONFLICT_CODES: ReadonlySet<string> = new Set([
  'VERSION_CONFLICT',
  'REQUEST_CONFLICT',
  'APPROVAL_REQUIRED',
  'REVIEW_STALE',
  'HISTORY_CONFLICT',
  'DATABASE_BUSY',
])

const UNPROCESSABLE_CODES: ReadonlySet<string> = new Set([
  'VALIDATION_FAILED',
  'SQL_UNSUPPORTED',
  'MIGRATION_IRREVERSIBLE',
  'INVALID_MIGRATION_ORDER',
  'BATCH_UNSAFE',
])

export function classifyWorkflowFailure(failure: ApiFailure): WorkflowFailure {
  const base = { code: null, fieldErrors: {}, reload: false, retrySameRequest: false }
  if (failure.kind === 'network') return { ...base, kind: 'network' }
  if (failure.kind === 'invalid_response') return { ...base, kind: 'invalid_response' }

  const { code, field_errors } = failure.error
  const http = { ...base, code, fieldErrors: field_errors }
  if (code === 'CSRF_INVALID' || code === 'CSRF_FAILED') return { ...http, kind: 'csrf' }

  switch (failure.status) {
    case 400:
      return { ...http, kind: 'invalid_request' }
    case 401:
      return { ...http, kind: 'session_ended' }
    case 403:
      return { ...http, kind: 'forbidden' }
    case 404:
      return { ...http, kind: 'not_found' }
    case 409:
      return { ...http, kind: 'conflict', reload: true }
    case 422:
      return { ...http, kind: 'validation' }
    case 501:
      return { ...http, kind: 'not_implemented' }
    case 502:
      return { ...http, kind: 'target_unavailable' }
    case 503:
      return code === 'QUEUE_UNAVAILABLE'
        ? { ...http, kind: 'queue_unavailable', retrySameRequest: true }
        : { ...http, kind: 'server' }
  }
  if (CONFLICT_CODES.has(code)) return { ...http, kind: 'conflict', reload: true }
  if (UNPROCESSABLE_CODES.has(code)) return { ...http, kind: 'validation' }
  return { ...http, kind: 'server' }
}
