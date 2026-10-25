import type { DatabaseSummary, ID, User } from '../api/types'

export const USERS: Record<string, User> = {
  demo_admin: { id: '10000000-0000-4000-8000-000000000001', username: 'demo_admin', is_admin: true, is_active: true },
  demo_viewer: { id: '10000000-0000-4000-8000-000000000002', username: 'demo_viewer', is_admin: false, is_active: true },
  demo_editor: { id: '10000000-0000-4000-8000-000000000003', username: 'demo_editor', is_admin: false, is_active: true },
  demo_outsider: { id: '10000000-0000-4000-8000-000000000004', username: 'demo_outsider', is_admin: false, is_active: true },
  demo_disabled: { id: '10000000-0000-4000-8000-000000000005', username: 'demo_disabled', is_admin: false, is_active: false },
}

export const TARGETS = {
  demo_main: { id: '30000000-0000-4000-8000-000000000001', name: 'demo_main' },
  demo_second: { id: '30000000-0000-4000-8000-000000000002', name: 'demo_second' },
} satisfies Record<string, { id: ID; name: string }>

export const DATABASES_BY_USER: Record<string, DatabaseSummary[]> = {
  demo_admin: [], 
  demo_viewer: [{ ...TARGETS.demo_main, access: 'view' }],
  demo_editor: [
    { ...TARGETS.demo_main, access: 'edit' },
    { ...TARGETS.demo_second, access: 'view' },
  ],
  demo_outsider: [],
  demo_disabled: [], 
}

export const FIXTURE_CSRF_TOKEN = 'fixture-csrf-token'

export const SESSION_LIFETIME_MS = 8 * 60 * 60 * 1000
