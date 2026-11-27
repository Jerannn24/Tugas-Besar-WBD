import { useEffect, useId, useState } from 'react'
import { Link, NavLink, Outlet, useLocation, useNavigate, useNavigation } from 'react-router'
import { clearCsrfToken, errorMessage, type ApiFailure } from '../api/client'
import { getSession, logout } from '../api/endpoints'
import type { SessionView } from '../api/types'
import { APP_NAME } from '../lib/app'
import { paths } from '../lib/paths'
import { SessionContext } from '../lib/session'
import { Alert } from './Alert'
import { Button } from './Button'
import { Loading } from './Loading'

type SessionState =
  | { status: 'loading' }
  | { status: 'ready'; session: SessionView }
  | { status: 'expired' }
  | { status: 'error'; failure: ApiFailure }
  | { status: 'incomplete' }

const NAV: { to: string; label: string; admin: boolean }[] = [
  { to: paths.databases, label: 'Databases', admin: false },
  { to: paths.adminUsers, label: 'Admin: Users', admin: true },
  { to: paths.adminGroups, label: 'Admin: Groups', admin: true },
  { to: paths.adminConnections, label: 'Admin: Connections', admin: true },
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

export function Shell() {
  const navigate = useNavigate()
  const pageLoading = useNavigation().state === 'loading'
  const [state, setState] = useState<SessionState>({ status: 'loading' })
  const [logoutPending, setLogoutPending] = useState(false)
  const [logoutError, setLogoutError] = useState<string | null>(null)
  const { pathname } = useLocation()
  const [menuOpenAt, setMenuOpenAt] = useState<string | null>(null)
  const menuOpen = menuOpenAt === pathname
  const menuId = useId()

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
      clearCsrfToken()
      navigate(paths.login, { replace: true })
      return
    }
    setLogoutPending(false)
    if (!(res.kind === 'http' && res.status === 401)) setLogoutError(errorMessage(res))
  }

  const session = state.status === 'ready' ? state.session : null

  return (
    <div className="page">
      <header className="navbar navbar-expand-lg d-print-none border-bottom app-navbar">
        <div className="container-xl">
          <Link className="navbar-brand me-3" to={paths.databases}>
            {APP_NAME}
          </Link>
          {session && (
            <button
              type="button"
              className="navbar-toggler"
              aria-controls={menuId}
              aria-expanded={menuOpen}
              aria-label={menuOpen ? 'Tutup menu' : 'Buka menu'}
              onClick={() => setMenuOpenAt(menuOpen ? null : pathname)}
            >
              <span className="navbar-toggler-icon" aria-hidden="true" />
            </button>
          )}
          {session && (
            <div id={menuId} className={`collapse navbar-collapse${menuOpen ? ' show' : ''}`}>
              <nav aria-label="Menu utama" className="me-lg-auto">
                <ul className="navbar-nav">
                  {NAV.filter((n) => !n.admin || session.user.is_admin).map((n) => (
                    <li key={n.to} className="nav-item">
                      <NavLink className="nav-link" to={n.to} onClick={() => setMenuOpenAt(null)}>
                        {n.label}
                      </NavLink>
                    </li>
                  ))}
                </ul>
              </nav>
              <div className="app-user">
                <span className="text-secondary">
                  Masuk sebagai <strong className="text-break">{session.user.username}</strong>
                  {session.user.is_admin && <span className="badge bg-blue-lt ms-2">Platform Admin</span>}
                </span>
                <Button variant="outline-secondary" pending={logoutPending} onClick={handleLogout}>
                  Logout
                </Button>
              </div>
            </div>
          )}
        </div>
      </header>

      <main className="page-wrapper">
        <div className="container-xl py-3">
          {logoutError && <Alert variant="danger" title="Logout gagal">{logoutError}</Alert>}
          {state.status === 'loading' && <Loading label="Memeriksa sesi…" />}
          {state.status === 'expired' && (
            <Alert variant="warning" title="Sesi berakhir">
              Mengalihkan ke <Link to={paths.login}>halaman login</Link>…
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
          {session && pageLoading && <Loading label="Memuat halaman…" />}
          {session && (
            <SessionContext.Provider value={session}>
              <Outlet />
            </SessionContext.Provider>
          )}
        </div>
      </main>
    </div>
  )
}
