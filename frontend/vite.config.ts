import { defineConfig, loadEnv, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import { resolve } from 'node:path'

const root = import.meta.dirname
const pages = {
  login: 'login/index.html',
  databases: 'databases/index.html',
  database: 'database/index.html',
  migrations: 'migrations/index.html',
  migration: 'migration/index.html',
  'admin-users': 'admin/users/index.html',
  'admin-groups': 'admin/groups/index.html',
  'admin-connections': 'admin/connections/index.html',
}

const devRootRedirect: Plugin = {
  name: 'dev-root-redirect',
  apply: 'serve',
  configureServer(server) {
    server.middlewares.use((req, res, next) => {
      if (req.url === '/' || req.url === '/index.html') {
        res.statusCode = 302
        res.setHeader('Location', '/login/')
        res.end()
        return
      }
      next()
    })
  },
}

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, root, '')
  const apiTarget = env.API_PROXY_TARGET || 'http://localhost:8080'

  return {
    plugins: [react(), devRootRedirect],
    appType: 'mpa',
    base: '/',
    server: {
      proxy: {
        '/api': { target: apiTarget, changeOrigin: false },
      },
    },
    build: {
      outDir: 'dist',
      assetsDir: 'assets',
      emptyOutDir: true,
      rollupOptions: {
        input: Object.fromEntries(
          Object.entries(pages).map(([name, file]) => [name, resolve(root, file)]),
        ),
      },
    },
  }
})
