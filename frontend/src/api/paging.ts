export const PAGE_LIMIT = 20

export type PageQuery = { cursor?: string | null; limit?: number }

export function pageParams(q: PageQuery = {}): URLSearchParams {
  const params = new URLSearchParams({ limit: String(q.limit ?? PAGE_LIMIT) })
  if (q.cursor) params.set('cursor', q.cursor)
  return params
}
