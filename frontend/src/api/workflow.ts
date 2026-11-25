import { request, type ApiResult } from './client'
import { pageParams, type PageQuery } from './paging'
import type {
  ApprovalRequest,
  CursorPage,
  Draft,
  DraftInputM2,
  DraftUpdateM2,
  EnqueueRequest,
  ID,
  JobAccepted,
  Proposal,
  ProposalCreate,
  RevisionRequest,
} from './types'

const e = encodeURIComponent
const db = (databaseId: ID) => `/api/databases/${e(databaseId)}`

export const createMigrationDraft = (databaseId: ID, input: DraftInputM2) => request<Draft>('POST', `${db(databaseId)}/drafts`, { body: input })
export const updateMigrationDraft = (databaseId: ID, draftId: ID, input: DraftUpdateM2) => request<Draft>('PATCH', `${db(databaseId)}/drafts/${e(draftId)}`, { body: input })

export const listProposals = (databaseId: ID, q?: PageQuery) => request<CursorPage<Proposal>>('GET', `${db(databaseId)}/proposals?${pageParams(q)}`)
export const getProposal = (databaseId: ID, proposalId: ID) => request<Proposal>('GET', `${db(databaseId)}/proposals/${e(proposalId)}`)
export const createProposal = (databaseId: ID, input: ProposalCreate) => request<Proposal>('POST', `${db(databaseId)}/proposals`, { body: input })
export const submitReview = (databaseId: ID, proposalId: ID, input: RevisionRequest) => request<Proposal>('POST', `${db(databaseId)}/proposals/${e(proposalId)}/submit-review`, { body: input })
export const approveProposal = (databaseId: ID, proposalId: ID, input: ApprovalRequest) => request<Proposal>('POST', `${db(databaseId)}/proposals/${e(proposalId)}/approvals`, { body: input })
export const enqueueProposal = (databaseId: ID, proposalId: ID, input: EnqueueRequest) => request<JobAccepted>('POST', `${db(databaseId)}/proposals/${e(proposalId)}/jobs`, { body: input })

// 200 = replay intent yang sama, 202 = job baru.
export const isEnqueueReplay = (res: ApiResult<JobAccepted>): boolean => res.ok && res.status === 200
export const newRequestId = (): string => crypto.randomUUID()
