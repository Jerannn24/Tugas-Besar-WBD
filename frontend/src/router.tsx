import { Navigate, createBrowserRouter } from 'react-router'
import { Loading } from './components/Loading'
import { Shell } from './components/Shell'
import { paths } from './lib/paths'
import { RouteErrorPage } from './pages/error'

const page = {
  login: () => import('./pages/login/page'),
  databases: () => import('./pages/databases/page'),
  database: () => import('./pages/database/page'),
  migrations: () => import('./pages/migrations/page'),
  migration: () => import('./pages/migration/page'),
  proposals: () => import('./pages/proposals/page'),
  jobs: () => import('./pages/jobs/page'),
  users: () => import('./pages/admin-users/page'),
  groups: () => import('./pages/admin-groups/page'),
  connections: () => import('./pages/admin-connections/page'),
  legacy: () => import('./pages/legacy'),
  missing: () => import('./pages/missing'),
}

export const router = createBrowserRouter([
  {
    errorElement: <RouteErrorPage />,
    hydrateFallbackElement: <Loading label="Memuat halaman…" />,
    children: [
      { path: 'login', lazy: () => page.login().then((m) => ({ Component: m.LoginPage })) },
      {
        element: <Shell />,
        children: [
          { index: true, element: <Navigate to={paths.databases} replace /> },
          { path: 'databases', lazy: () => page.databases().then((m) => ({ Component: m.DatabasesPage })) },
          { path: 'databases/:databaseId', lazy: () => page.database().then((m) => ({ Component: m.DatabasePage })) },
          {
            path: 'databases/:databaseId/migrations',
            lazy: () => page.migrations().then((m) => ({ Component: m.MigrationsPage })),
          },
          {
            path: 'databases/:databaseId/migrations/:executionId',
            lazy: () => page.migration().then((m) => ({ Component: m.ExecutionPage })),
          },
          {
            path: 'databases/:databaseId/drafts/new',
            lazy: () => page.migration().then((m) => ({ Component: m.NewDraftPage })),
          },
          {
            path: 'databases/:databaseId/drafts/:draftId',
            lazy: () => page.migration().then((m) => ({ Component: m.DraftPage })),
          },
          {
            path: 'databases/:databaseId/proposals',
            lazy: () => page.proposals().then((m) => ({ Component: m.ProposalsPage })),
          },
          {
            path: 'databases/:databaseId/proposals/:proposalId',
            lazy: () => page.proposals().then((m) => ({ Component: m.ProposalPage })),
          },
          {
            path: 'databases/:databaseId/jobs',
            lazy: () => page.jobs().then((m) => ({ Component: m.JobsPage })),
          },
          {
            path: 'databases/:databaseId/attempts',
            lazy: () => page.jobs().then((m) => ({ Component: m.AttemptsPage })),
          },
          {
            path: 'databases/:databaseId/jobs/:jobId',
            lazy: () => page.jobs().then((m) => ({ Component: m.JobPage })),
          },
          { path: 'admin/users', lazy: () => page.users().then((m) => ({ Component: m.UsersPage })) },
          { path: 'admin/groups', lazy: () => page.groups().then((m) => ({ Component: m.GroupsPage })) },
          { path: 'admin/connections', lazy: () => page.connections().then((m) => ({ Component: m.ConnectionsPage })) },
          { path: 'database', lazy: () => page.legacy().then((m) => ({ Component: m.LegacyDatabase })) },
          { path: 'migrations', lazy: () => page.legacy().then((m) => ({ Component: m.LegacyMigrations })) },
          { path: 'migration', lazy: () => page.legacy().then((m) => ({ Component: m.LegacyMigration })) },
          { path: '*', lazy: () => page.missing().then((m) => ({ Component: m.MissingPage })) },
        ],
      },
    ],
  },
])
