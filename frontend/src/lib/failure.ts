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
