import { StrictMode, type ReactNode } from 'react'
import { createRoot } from 'react-dom/client'
import '@tabler/core/dist/css/tabler.min.css'
import '../styles/app.css'

export function mountPage(node: ReactNode): void {
  const container = document.getElementById('root')
  
  if (!container) throw new Error('#root tidak ditemukan')
  createRoot(container).render(<StrictMode>{node}</StrictMode>)
}
