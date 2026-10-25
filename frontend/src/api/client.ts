import type { ApiErrorBody } from './types'

export type HttpMethod = 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE'

export type ApiResult<T> =
  | { ok: true; status: number; data: T }
  | { ok: false; kind: 'http'; status: number; error: ApiErrorBody }
  | { ok: false; kind: 'network' }
  | { ok: false; kind: 'invalid_response'; status: number }

export type ApiFailure = Exclude<ApiResult<unknown>, { ok: true }>

export type RequestOptions = {
  body?: unknown
  redirectOn401?: boolean
  timeoutMs?: number
}

export type TransportRequest = {
  method: HttpMethod
  path: string
  headers: Record<string, string>
  body?: string
  signal?: AbortSignal
}

const MUTATING: ReadonlySet<HttpMethod> = new Set(['POST', 'PATCH', 'PUT', 'DELETE'])
const LOGIN_PAGE = '/login/'

const useFixtures = import.meta.env.DEV && import.meta.env.VITE_USE_FIXTURES === 'true'

async function transport(req: TransportRequest): Promise<Response> {
  if (import.meta.env.DEV && useFixtures) {
    const { handleFixtureRequest } = await import('../fixtures')
    return handleFixtureRequest(req)
  }

  return fetch(req.path, {
    method: req.method,
    headers: req.headers,
    body: req.body,
    credentials: 'same-origin',
    signal: req.signal,
  })
}

type CsrfSource = 'unknown' | 'body' | 'cookie'

const CSRF_COOKIE = 'csrf_token'

let csrfSource: CsrfSource = 'unknown'
let csrfToken: string | null = null
let csrfInFlight: Promise<ApiResult<string>> | null = null

function readCsrfCookie(): string | null {
  for (const part of document.cookie.split(';')) {
    const [name, ...rest] = part.trim().split('=')
    if (name === CSRF_COOKIE) {
      const value = rest.join('=')
      return value === '' ? null : decodeURIComponent(value)
    }
  }
  return null
}

const MISSING_TOKEN: ApiResult<string> = { ok: false, kind: 'invalid_response', status: 200 }

export async function getCsrfToken(): Promise<ApiResult<string>> {
  if (csrfSource === 'body' && csrfToken !== null) return { ok: true, status: 200, data: csrfToken }
  if (csrfSource === 'cookie') {
    const cookie = readCsrfCookie()
    if (cookie !== null) return { ok: true, status: 200, data: cookie }
  }
  return refreshCsrfToken()
}

export function refreshCsrfToken(): Promise<ApiResult<string>> {
  csrfToken = null
  if (csrfInFlight) return csrfInFlight

  csrfInFlight = send<{ csrf_token?: unknown }>('GET', '/api/csrf', { redirectOn401: false })
    .then((res): ApiResult<string> => {
      if (!res.ok) return res
      const fromBody = isObject(res.data) ? res.data.csrf_token : undefined
      if (typeof fromBody === 'string' && fromBody !== '') {
        csrfSource = 'body'
        csrfToken = fromBody
        return { ok: true, status: res.status, data: fromBody }
      }
      csrfSource = 'cookie'
      const cookie = readCsrfCookie()
      return cookie === null ? MISSING_TOKEN : { ok: true, status: res.status, data: cookie }
    })
    .finally(() => {
      csrfInFlight = null
    })

  return csrfInFlight
}

export function clearCsrfToken(): void {
  csrfToken = null
}

export async function request<T>(
  method: HttpMethod,
  path: string,
  options: RequestOptions = {},
): Promise<ApiResult<T>> {
  const extraHeaders: Record<string, string> = {}
  if (MUTATING.has(method)) {
    const token = await getCsrfToken()
    
    if (!token.ok) return token
    extraHeaders['X-CSRF-Token'] = token.data
  }

  const res = await send<T>(method, path, options, extraHeaders)

  if (!res.ok && res.kind === 'http') {
    if (res.status === 403 && isCsrfError(res)) {
      void refreshCsrfToken()
    }
  }

  return res
}

async function send<T>(
  method: HttpMethod,
  path: string,
  options: RequestOptions,
  extraHeaders: Record<string, string> = {},
): Promise<ApiResult<T>> {
  const headers: Record<string, string> = { Accept: 'application/json', ...extraHeaders }
  let body: string | undefined

  if (options.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    body = JSON.stringify(options.body)
  }

  const controller = options.timeoutMs !== undefined ? new AbortController() : null
  const timer = controller ? setTimeout(() => controller.abort(), options.timeoutMs) : undefined

  let response: Response
  try {
    response = await transport({ method, path, headers, body, signal: controller?.signal })
  } catch {
    clearTimeout(timer)
    return { ok: false, kind: 'network' }
  }

  let payload: unknown
  try {
    payload = await response.json()
  } catch {
    clearTimeout(timer)
    if (controller?.signal.aborted) return { ok: false, kind: 'network' }
    return { ok: false, kind: 'invalid_response', status: response.status }
  }
  clearTimeout(timer)

  if (response.ok && isObject(payload) && 'data' in payload) {
    return { ok: true, status: response.status, data: payload.data as T }
  }

  const error = !response.ok && isObject(payload) ? toErrorBody(payload.error) : null
  if (error) {
    if (response.status === 401 && options.redirectOn401 !== false) {
      window.location.assign(LOGIN_PAGE)
    }
    return { ok: false, kind: 'http', status: response.status, error }
  }

  return { ok: false, kind: 'invalid_response', status: response.status }
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null
}

function toErrorBody(v: unknown): ApiErrorBody | null {
  if (!isObject(v) || typeof v.code !== 'string' || typeof v.message !== 'string') return null
  const fieldErrors: Record<string, string> = {}
  if (isObject(v.field_errors)) {
    for (const [key, value] of Object.entries(v.field_errors)) {
      if (typeof value === 'string') fieldErrors[key] = value
    }
  }
  return {
    code: v.code,
    message: v.message,
    field_errors: fieldErrors,
    retryable: typeof v.retryable === 'boolean' ? v.retryable : false,
  }
}

const GENERIC_MESSAGES: Record<string, string> = {
  UNAUTHENTICATED: 'Sesi berakhir. Mengalihkan ke halaman login…',
  INVALID_CREDENTIALS: 'Username atau password salah.',
  FORBIDDEN: 'Anda tidak memiliki akses.',
  CSRF_INVALID: 'Sesi keamanan diperbarui, silakan kirim ulang.',
  NOT_FOUND: 'Data tidak ditemukan.',
  VERSION_CONFLICT: 'Data telah diubah di tempat lain. Muat ulang sebelum menyimpan.',
  DATABASE_BUSY: 'Database target sedang menjalankan migration. Coba lagi nanti.',
  TARGET_UNAVAILABLE: 'Database target tidak dapat dihubungi.',
  INTERNAL_ERROR: 'Terjadi kesalahan pada server.',
}

export function errorMessage(failure: ApiFailure): string {
  switch (failure.kind) {
    case 'network':
      return 'Tidak dapat menghubungi server. Periksa koneksi lalu coba lagi.'
    case 'invalid_response':
      return 'Respons server tidak dikenali.'
    case 'http': {
      const { code, message } = failure.error
      if (isCsrfError(failure)) return GENERIC_MESSAGES.CSRF_INVALID
      if (code === 'INVALID_CREDENTIALS' || code === 'INTERNAL_ERROR') {
        return GENERIC_MESSAGES[code]
      }

      return message || GENERIC_MESSAGES[code] || 'Permintaan gagal.'
    }
  }
}

export function isErrorCode(result: ApiResult<unknown>, code: string): boolean {
  return !result.ok && result.kind === 'http' && result.error.code === code
}

const CSRF_CODES: ReadonlySet<string> = new Set(['CSRF_INVALID', 'CSRF_FAILED'])

export function isCsrfError(result: ApiResult<unknown>): boolean {
  return !result.ok && result.kind === 'http' && CSRF_CODES.has(result.error.code)
}
