import type { ID } from '../api/types'

function withQuery(path: string, params: Record<string, string>): string {
  const query = new URLSearchParams(params).toString()
  return query === '' ? path : `${path}?${query}`
}

const db = (id: ID) => `/databases/${encodeURIComponent(id)}`

export const paths = {
  login: '/login',
  databases: '/databases',
  database: (id: ID, table?: { schema: string; table: string }) => table ? withQuery(db(id), { schema: table.schema, table: table.table }) : db(id),
  migrations: (id: ID) => `${db(id)}/migrations`,
  execution: (id: ID, executionId: ID) => `${db(id)}/migrations/${encodeURIComponent(executionId)}`,
  newDraft: (id: ID) => `${db(id)}/drafts/new`,
  draft: (id: ID, draftId: ID) => `${db(id)}/drafts/${encodeURIComponent(draftId)}`,
  proposals: (id: ID) => `${db(id)}/proposals`,
  proposal: (id: ID, proposalId: ID) => `${db(id)}/proposals/${encodeURIComponent(proposalId)}`,
  jobs: (id: ID) => `${db(id)}/jobs`,
  attempts: (id: ID) => `${db(id)}/attempts`,
  job: (id: ID, jobId: ID) => `${db(id)}/jobs/${encodeURIComponent(jobId)}`,
  adminUsers: '/admin/users',
  adminGroups: '/admin/groups',
  adminConnections: '/admin/connections',
} as const
