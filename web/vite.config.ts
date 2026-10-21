import path from 'node:path'
import fs from 'node:fs'
import { defineConfig, loadEnv, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

const outDir = path.resolve(import.meta.dirname, '../internal/webui/dist')

// The repo gitignores dist contents except .gitkeep, and emptyOutDir wipes it
// on every build, so it has to be recreated afterwards.
function keepDist(): Plugin {
  return {
    name: 'gotero-keep-dist',
    apply: 'build',
    closeBundle() {
      fs.mkdirSync(outDir, { recursive: true })
      fs.writeFileSync(path.join(outDir, '.gitkeep'), '')
      // The MSW worker is dev-only; never ship it inside the server binary.
      fs.rmSync(path.join(outDir, 'mockServiceWorker.js'), { force: true })
    },
  }
}

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, import.meta.dirname, '')
  const target = env.VITE_API_URL || 'http://localhost:8080'
  return {
    plugins: [react(), tailwindcss(), keepDist()],
    resolve: { alias: { '@': path.resolve(import.meta.dirname, 'src') } },
    build: {
      outDir,
      emptyOutDir: true,
      // Served from the same host as the API, so a ~650 kB vendor chunk is acceptable.
      chunkSizeWarningLimit: 800,
      rolldownOptions: {
        output: {
          // Keeps the app chunk small so UI changes do not invalidate the cached vendor code.
          codeSplitting: { groups: [{ name: 'vendor', test: /node_modules/ }] },
        },
      },
    },
    server: {
      // Only reached when mocking is off; with MSW active the worker answers first.
      proxy: { '/admin/api': { target, changeOrigin: false } },
    },
    test: {
      globals: true,
      environment: 'jsdom',
      setupFiles: ['./src/test/setup.ts'],
      css: false,
    },
  }
})
