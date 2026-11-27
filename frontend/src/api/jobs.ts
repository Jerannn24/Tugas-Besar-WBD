import { request } from './client'
import { pageParams, type PageQuery } from './paging'
import type { Attempt, CursorPage, ID, JobView } from './types'

const e = encodeURIComponent
const db = (databaseId: ID) => `/api/databases/${e(databaseId)}`

export const listJobs = (databaseId: ID, q?: PageQuery) => request<CursorPage<JobView>>('GET', `${db(databaseId)}/jobs?${pageParams(q)}`)
export const getJob = (databaseId: ID, jobId: ID) => request<JobView>('GET', `${db(databaseId)}/jobs/${e(jobId)}`)
export const listAttempts = (databaseId: ID, q?: PageQuery) => request<CursorPage<Attempt>>('GET', `${db(databaseId)}/attempts?${pageParams(q)}`)
