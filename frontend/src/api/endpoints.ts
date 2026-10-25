import { refreshCsrfToken, request } from './client'
import type {
  ConnectionInput,
  ConnectionPatchInput,
  ConnectionSummary,
  ConnectionTestResult,
  CsrfView,
  DatabaseSummary,
  DeletedResult,
  Draft,
  DraftCreateInput,
  DraftPatchInput,
  ExecuteInput,
  Execution,
  Group,
  GroupGrants,
  GroupGrantsInput,
  GroupInput,
  GroupMembers,
  GroupMembersInput,
  ID,
  List,
  LoginInput,
  LogoutResult,
  RowsQuery,
  SchemaSummary,
  SessionView,
  TableMetadata,
  TableRows,
  User,
  UserCreateInput,
  UserPatchInput,
} from './types'

const e = encodeURIComponent
const db = (databaseId: ID) => `/api/databases/${e(databaseId)}`

export const getCsrf = () => request<CsrfView>('GET', '/api/csrf', { redirectOn401: false })

export async function login(input: LoginInput) {
  const res = await request<SessionView>('POST', '/api/login', { body: input, redirectOn401: false })
  if (res.ok) await refreshCsrfToken()
  return res
}

export const getSession = (options: { redirectOn401?: boolean } = {}) =>
  request<SessionView>('GET', '/api/session', options)
export const logout = (options: { redirectOn401?: boolean } = {}) =>
  request<LogoutResult>('POST', '/api/logout', { body: {}, ...options })

export const listUsers = () => request<List<User>>('GET', '/api/admin/users')
export const createUser = (input: UserCreateInput) =>
  request<User>('POST', '/api/admin/users', { body: input })
export const updateUser = (userId: ID, input: UserPatchInput) =>
  request<User>('PATCH', `/api/admin/users/${e(userId)}`, { body: input })

export const listGroups = () => request<List<Group>>('GET', '/api/admin/groups')
export const createGroup = (input: GroupInput) =>
  request<Group>('POST', '/api/admin/groups', { body: input })
export const updateGroup = (groupId: ID, input: GroupInput) =>
  request<Group>('PATCH', `/api/admin/groups/${e(groupId)}`, { body: input })
export const deleteGroup = (groupId: ID) =>
  request<DeletedResult>('DELETE', `/api/admin/groups/${e(groupId)}`)
export const getGroupMembers = (groupId: ID) =>
  request<GroupMembers>('GET', `/api/admin/groups/${e(groupId)}/members`)
export const putGroupMembers = (groupId: ID, input: GroupMembersInput) =>
  request<GroupMembers>('PUT', `/api/admin/groups/${e(groupId)}/members`, { body: input })
export const getGroupGrants = (groupId: ID) =>
  request<GroupGrants>('GET', `/api/admin/groups/${e(groupId)}/grants`)
export const putGroupGrants = (groupId: ID, input: GroupGrantsInput) =>
  request<GroupGrants>('PUT', `/api/admin/groups/${e(groupId)}/grants`, { body: input })

export const listDatabases = () => request<List<DatabaseSummary>>('GET', '/api/databases')
export const listConnections = () => request<List<ConnectionSummary>>('GET', '/api/admin/connections')
export const createConnection = (input: ConnectionInput) =>
  request<ConnectionSummary>('POST', '/api/admin/connections', { body: input })
export const updateConnection = (databaseId: ID, input: ConnectionPatchInput) =>
  request<ConnectionSummary>('PATCH', `/api/admin/connections/${e(databaseId)}`, { body: input })
export const testConnection = (databaseId: ID) =>
  request<ConnectionTestResult>('POST', `/api/admin/connections/${e(databaseId)}/test`, { body: {} })
export const deleteConnection = (databaseId: ID) =>
  request<DeletedResult>('DELETE', `/api/admin/connections/${e(databaseId)}`)

export const listSchemas = (databaseId: ID) =>
  request<List<SchemaSummary>>('GET', `${db(databaseId)}/schemas`)
export const getTable = (databaseId: ID, schema: string, table: string) =>
  request<TableMetadata>('GET', `${db(databaseId)}/table?${new URLSearchParams({ schema, table })}`)
export function getRows(databaseId: ID, q: RowsQuery) {
  const params = new URLSearchParams({
    schema: q.schema,
    table: q.table,
    limit: String(q.limit),
    offset: String(q.offset),
  })
  return request<TableRows>('GET', `${db(databaseId)}/rows?${params}`)
}

export const listDrafts = (databaseId: ID) => 
  request<List<Draft>>('GET', `${db(databaseId)}/drafts`)
export const createDraft = (databaseId: ID, input: DraftCreateInput) =>
  request<Draft>('POST', `${db(databaseId)}/drafts`, { body: input })
export const getDraft = (databaseId: ID, draftId: ID) =>
  request<Draft>('GET', `${db(databaseId)}/drafts/${e(draftId)}`)
export const updateDraft = (databaseId: ID, draftId: ID, input: DraftPatchInput) =>
  request<Draft>('PATCH', `${db(databaseId)}/drafts/${e(draftId)}`, { body: input })
export const listMigrations = (databaseId: ID) =>
  request<List<Execution>>('GET', `${db(databaseId)}/migrations`)
export const getMigration = (databaseId: ID, executionId: ID) =>
  request<Execution>('GET', `${db(databaseId)}/migrations/${e(executionId)}`)
export const executeDraft = (databaseId: ID, draftId: ID, input: ExecuteInput) =>
  request<Execution>('POST', `${db(databaseId)}/drafts/${e(draftId)}/execute`, {
    body: input,
    timeoutMs: 30_000,
  })
