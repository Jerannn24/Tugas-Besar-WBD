import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { RouterProvider } from 'react-router/dom'
import '@tabler/core/dist/css/tabler.min.css'
import './styles/app.css'
import { reloadOnBfcacheRestore } from './lib/bfcache'
import { router } from './router'

const container = document.getElementById('root')
if (!container) throw new Error('#root tidak ditemukan')
  
reloadOnBfcacheRestore()
createRoot(container).render(
  <StrictMode>
    <RouterProvider router={router} />
  </StrictMode>,
)
