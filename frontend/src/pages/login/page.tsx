import { useEffect, useState, type FormEvent } from 'react'
import { useNavigate, useSearchParams } from 'react-router'
import { errorMessage, isCsrfError, refreshCsrfToken, type ApiFailure } from '../../api/client'
import { getSession, login } from '../../api/endpoints'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { FormField } from '../../components/FormField'
import { Loading } from '../../components/Loading'
import { Panel } from '../../components/Panel'
import { APP_NAME, SESSION_ENDED_PARAM, SESSION_ENDED_VALUE } from '../../lib/app'
import { paths } from '../../lib/paths'

type Feedback =
  | { variant: 'danger' | 'warning'; title: string; message: string }
  | null

type CsrfState = { status: 'loading' } | { status: 'ready' } | { status: 'error'; failure: ApiFailure }

export function LoginPage() {
  const navigate = useNavigate()
  const [searchParams] = useSearchParams()
  const sessionEnded = searchParams.get(SESSION_ENDED_PARAM) === SESSION_ENDED_VALUE
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [pending, setPending] = useState(false)
  const [feedback, setFeedback] = useState<Feedback>(null)
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [csrf, setCsrf] = useState<CsrfState>({ status: 'loading' })
  const [checkingSession, setCheckingSession] = useState(true)

  async function loadCsrf() {
    setCsrf({ status: 'loading' })
    const res = await refreshCsrfToken()
    setCsrf(res.ok ? { status: 'ready' } : { status: 'error', failure: res })
  }

  useEffect(() => {
    let active = true
    getSession({ redirectOn401: false }).then((res) => {
      if (!active) return
      if (res.ok) {
        navigate(paths.databases, { replace: true })
        return
      }
      setCheckingSession(false)
      void loadCsrf()
    })
    return () => {
      active = false
    }
  }, [navigate])

  useEffect(() => {
    document.title = `Login - ${APP_NAME}`
  }, [])

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (pending) return
    setPending(true)
    setFeedback(null)
    setFieldErrors({})

    const res = await login({ username, password })
    if (res.ok) {
      setPassword('')
      navigate(paths.databases, { replace: true })
      return
    }

    setPending(false)
    if (res.kind === 'http' && res.status === 401) {
      setPassword('')
      setFeedback({ variant: 'danger', title: 'Login gagal', message: 'Username atau password salah.' })
    } else if (isCsrfError(res)) {
      setFeedback({ variant: 'warning', title: 'Kirim ulang', message: errorMessage(res) })
    } else if (res.kind === 'http' && res.error.code === 'VALIDATION_FAILED') {
      setFieldErrors(res.error.field_errors)
      setFeedback({ variant: 'danger', title: 'Pastikan input benar', message: errorMessage(res) })
    } else {
      setFeedback({ variant: 'danger', title: 'Login gagal', message: errorMessage(res) })
    }
  }

  return (
    <main className="page page-center">
      <div className="container container-tight py-4">
        <h1 className="text-center mb-4">{APP_NAME}</h1>
        <Panel title="Login">
          {checkingSession ? (
            <Loading label="Memeriksa sesi…" />
          ) : (
            <>
              {csrf.status === 'error' && (
                <Alert variant="warning" title="Gagal menyiapkan sesi keamanan">
                  <p className="mb-2">{errorMessage(csrf.failure)}</p>
                  <Button variant="outline-secondary" onClick={() => void loadCsrf()}>
                    Coba lagi
                  </Button>
                </Alert>
              )}
              {sessionEnded && !feedback && (
                <Alert variant="info">
                  Sesi Anda berakhir atau hak akses Anda berubah. Silakan masuk kembali.
                </Alert>
              )}
              {feedback && (
                <Alert variant={feedback.variant} title={feedback.title}>
                  {feedback.message}
                </Alert>
              )}
              <form method="post" noValidate onSubmit={handleSubmit}>
                <FormField label="Username" required error={fieldErrors.username}>
                  {(control) => (
                    <input
                      {...control}
                      type="text"
                      name="username"
                      className={`form-control${fieldErrors.username ? ' is-invalid' : ''}`}
                      autoComplete="username"
                      autoFocus
                      required
                      value={username}
                      onChange={(e) => setUsername(e.target.value)}
                      disabled={pending}
                    />
                  )}
                </FormField>
                <FormField label="Password" required error={fieldErrors.password}>
                  {(control) => (
                    <input
                      {...control}
                      type="password"
                      name="password"
                      className={`form-control${fieldErrors.password ? ' is-invalid' : ''}`}
                      autoComplete="current-password"
                      required
                      value={password}
                      onChange={(e) => setPassword(e.target.value)}
                      disabled={pending}
                    />
                  )}
                </FormField>
                <Button
                  type="submit"
                  className="w-100"
                  pending={pending}
                  disabled={csrf.status === 'loading'}
                >
                  {pending ? 'Memproses…' : 'Masuk'}
                </Button>
              </form>
            </>
          )}
        </Panel>
      </div>
    </main>
  )
}
