import { defineConfig, loadEnv } from 'vite'
import react from '@vitejs/plugin-react'

const root = import.meta.dirname

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, root, '')
  const apiTarget = env.API_PROXY_TARGET || 'http://localhost:8080'

  return {
    plugins: [react()],
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
    },
  }
})
