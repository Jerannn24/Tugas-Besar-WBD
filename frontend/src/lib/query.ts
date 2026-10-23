export function getQueryParam(name: string): string | null {
  const value = new URLSearchParams(window.location.search).get(name)
  return value === null || value.trim() === '' ? null : value
}

export function pageUrl(path: string, params: Record<string, string>): string {
  return `${path}?${new URLSearchParams(params)}`
}
