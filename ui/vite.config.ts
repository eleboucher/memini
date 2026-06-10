import { defineConfig } from 'vite'
import preact from '@preact/preset-vite'
import { fileURLToPath, URL } from 'node:url'

// The UI is served by the Go binary from internal/api/ui/dist via go:embed,
// mounted at the server root. `base: './'` makes every asset reference relative
// so the bundle works regardless of the mount path.
export default defineConfig({
  plugins: [preact()],
  base: './',
  build: {
    outDir: fileURLToPath(new URL('../internal/api/ui/dist', import.meta.url)),
    emptyOutDir: true,
    chunkSizeWarningLimit: 900,
  },
  server: {
    // `mise run ui-dev` proxies API calls to a locally running memini.
    proxy: {
      '/v1': 'http://localhost:8080',
    },
  },
})
