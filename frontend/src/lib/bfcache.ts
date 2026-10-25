import { reloadWithoutWarning } from './unload'

let remove: (() => void) | null = null

export function reloadOnBfcacheRestore(): () => void {
  if (remove) return remove
  const handler = (event: PageTransitionEvent) => {
    if (!event.persisted) return
    remove?.()
    reloadWithoutWarning()
  }
  window.addEventListener('pageshow', handler)
  remove = () => {
    window.removeEventListener('pageshow', handler)
    remove = null
  }
  return remove
}
