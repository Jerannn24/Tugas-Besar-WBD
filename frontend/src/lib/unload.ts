import { useEffect } from 'react'

let bypass = false

export function useBeforeUnload(active: boolean): void {
  useEffect(() => {
    if (!active) return
    const handler = (event: BeforeUnloadEvent) => {
      if (bypass) return
      event.preventDefault()
    }
    window.addEventListener('beforeunload', handler)
    return () => window.removeEventListener('beforeunload', handler)
  }, [active])
}

export function assignWithoutWarning(url: string): void {
  bypass = true
  window.location.assign(url)
}

export function reloadWithoutWarning(): void {
  bypass = true
  window.location.reload()
}
