import type { ApiErrorBody } from '../api/types'
import type { TransportRequest } from '../api/client'

export function ok(data: unknown, status = 200): Response {
  return json(status, { data })
}

export function fail(
  status: number,
  code: string,
  message: string,
  field_errors: Record<string, string> = {},
  retryable = false,
): Response {
  const error: ApiErrorBody = { code, message, field_errors, retryable }
  return json(status, { error })
}

export function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' },
  })
}

export function parseBody<T>(req: TransportRequest): T | null {
  try {
    return req.body ? (JSON.parse(req.body) as T) : null
  } catch {
    return null
  }
}
