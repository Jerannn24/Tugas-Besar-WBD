import { useEffect, useState, type ReactNode } from 'react'
import { errorMessage, type ApiFailure } from '../api/client'
import { getSession, logout } from '../api/endpoints'
import type { SessionView } from '../api/types'
import { Alert } from './Alert'
import { Button } from './Button'
import { Loading } from './Loading'
import { APP_NAME } from '../lib/app'

export type ActivePage =
  | 'databases'
  | 'database'
  | 'migrations'
  | 'migration'
  | 'admin-users'
  | 'admin-groups'
  | 'admin-connections'

type Props = {
  activePage: ActivePage
  title: string
  children: ReactNode | ((session: SessionView) => ReactNode)
}

type SessionState =
  | { status: 'loading' }
  | { status: 'ready'; session: SessionView }
  | { status: 'expired' }
  | { status: 'error'; failure: ApiFailure }
  | { status: 'incomplete' }

const NAV: { page: ActivePage; href: string; label: string; admin: boolean }[] = [
  { page: 'databases', href: '/databases/', label: 'Databases', admin: false },
  { page: 'admin-users', href: '/admin/users/', label: 'Admin: Users', admin: true },
  { page: 'admin-groups', href: '/admin/groups/', label: 'Admin: Groups', admin: true },
  { page: 'admin-connections', href: '/admin/connections/', label: 'Admin: Connections', admin: true },
]

function hasUser(data: unknown): data is SessionView {
  if (typeof data !== 'object' || data === null || !('user' in data)) return false
  const user = data.user
  return (
    typeof user === 'object' &&
    user !== null &&
    'id' in user && typeof user.id === 'string' &&
    'username' in user && typeof user.username === 'string' &&
    'is_admin' in user && typeof user.is_admin === 'boolean'
  )
}

export function Layout({ activePage, title, children }: Props) {
  const [state, setState] = useState<SessionState>({ status: 'loading' })
  const [logoutPending, setLogoutPending] = useState(false)
  const [logoutError, setLogoutError] = useState<string | null>(null)

  useEffect(() => {
    let active = true
    getSession().then((res) => {
      if (!active) return
      if (res.ok) setState(hasUser(res.data) ? { status: 'ready', session: res.data } : { status: 'incomplete' })
      else if (res.kind === 'http' && res.status === 401) setState({ status: 'expired' })
      else setState({ status: 'error', failure: res })
    })
    return () => {
      active = false
    }
  }, [])

  async function handleLogout() {
    setLogoutPending(true)
    setLogoutError(null)
    const res = await logout()
    if (res.ok) {
      window.location.assign('/login/')
      return
    }
    setLogoutPending(false)
    if (!(res.kind === 'http' && res.status === 401)) setLogoutError(errorMessage(res))
  }

  const session = state.status === 'ready' ? state.session : null
  const isActive = (page: ActivePage) =>
    page === activePage ||
    (page === 'databases' && ['database', 'migrations', 'migration'].includes(activePage))

  return (
    <div className="page">
      <header className="navbar navbar-expand d-print-none border-bottom">
        <div className="container-xl flex-wrap gap-2">
          <a className="navbar-brand me-3" href="/databases/">
            {APP_NAME}
          </a>
          {session && (
            <nav aria-label="Menu utama" className="me-auto">
              <ul className="navbar-nav flex-wrap">
                {NAV.filter((n) => !n.admin || session.user.is_admin).map((n) => (
                  <li key={n.page} className={`nav-item${isActive(n.page) ? ' active' : ''}`}>
                    <a
                      className="nav-link"
                      href={n.href}
                      aria-current={isActive(n.page) ? 'page' : undefined}
                    >
                      {n.label}
                    </a>
                  </li>
                ))}
              </ul>
            </nav>
          )}
          {session && (
            <div className="d-flex align-items-center gap-2 ms-auto">
              <span className="text-secondary">
                Masuk sebagai <strong>{session.user.username}</strong>
                {session.user.is_admin && <span className="badge bg-blue-lt ms-2">Platform Admin</span>}
              </span>
              <Button variant="outline-secondary" pending={logoutPending} onClick={handleLogout}>
                Logout
              </Button>
            </div>
          )}
        </div>
      </header>

      <main className="page-wrapper">
        <div className="container-xl py-3">
          <h1 className="page-title mb-3">{title}</h1>
          {logoutError && <Alert variant="danger" title="Logout gagal">{logoutError}</Alert>}
          {state.status === 'loading' && <Loading label="Memeriksa sesi…" />}
          {state.status === 'expired' && (
            <Alert variant="warning" title="Sesi berakhir">
              Mengalihkan ke <a href="/login/">halaman login</a>…
            </Alert>
          )}
          {state.status === 'error' && (
            <Alert variant="danger" title="Gagal memuat sesi">
              {errorMessage(state.failure)}
            </Alert>
          )}
          {state.status === 'incomplete' && (
            <Alert variant="danger" title="Data sesi dari server tidak lengkap">
              Server tidak mengirim data user untuk sesi ini, sehingga halaman tidak dapat ditampilkan.
            </Alert>
          )}
          {session && (typeof children === 'function' ? children(session) : children)}
        </div>
      </main>
    </div>
  )
}
