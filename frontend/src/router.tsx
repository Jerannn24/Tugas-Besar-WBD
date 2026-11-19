import { Navigate, createBrowserRouter } from 'react-router'
import { Shell } from './components/Shell'
import { ConnectionsPage } from './pages/admin-connections/page'
import { GroupsPage } from './pages/admin-groups/page'
import { UsersPage } from './pages/admin-users/page'
import { DatabasePage } from './pages/database/page'
import { DatabasesPage } from './pages/databases/page'
import { LegacyDatabase, LegacyMigration, LegacyMigrations } from './pages/legacy'
import { LoginPage } from './pages/login/page'
import { DraftPage, ExecutionPage, NewDraftPage } from './pages/migration/page'
import { MigrationsPage } from './pages/migrations/page'
import { MissingPage, RouteErrorPage } from './pages/missing'
import { paths } from './lib/paths'

export const router = createBrowserRouter([
  {
    errorElement: <RouteErrorPage />,
    children: [
      { path: 'login', element: <LoginPage /> },
      {
        element: <Shell />,
        children: [
          { index: true, element: <Navigate to={paths.databases} replace /> },
          { path: 'databases', element: <DatabasesPage /> },
          { path: 'databases/:databaseId', element: <DatabasePage /> },
          { path: 'databases/:databaseId/migrations', element: <MigrationsPage /> },
          { path: 'databases/:databaseId/migrations/:executionId', element: <ExecutionPage /> },
          { path: 'databases/:databaseId/drafts/new', element: <NewDraftPage /> },
          { path: 'databases/:databaseId/drafts/:draftId', element: <DraftPage /> },
          { path: 'admin/users', element: <UsersPage /> },
          { path: 'admin/groups', element: <GroupsPage /> },
          { path: 'admin/connections', element: <ConnectionsPage /> },
          
          { path: 'database', element: <LegacyDatabase /> },
          { path: 'migrations', element: <LegacyMigrations /> },
          { path: 'migration', element: <LegacyMigration /> },
          { path: '*', element: <MissingPage /> },
        ],
      },
    ],
  },
])
