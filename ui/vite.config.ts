import { defineConfig, type Plugin } from 'vite'
import preact from '@preact/preset-vite'
import { fileURLToPath, URL } from 'node:url'
import { writeFileSync } from 'node:fs'
import { join } from 'node:path'

// The UI is served by the Go binary from internal/api/ui/dist via go:embed,
// mounted at the server root. `base: './'` makes every asset reference relative
// so the bundle works regardless of the mount path.
const outDir = fileURLToPath(new URL('../internal/api/ui/dist', import.meta.url))

// emptyOutDir wipes dist on every build, including the tracked .gitkeep that
// keeps `//go:embed all:dist` compiling on a clean checkout. Re-create it after
// each build so the build never leaves the file (and CI) in a broken state.
const keepGitkeep: Plugin = {
  name: 'keep-dist-gitkeep',
  closeBundle() {
    writeFileSync(join(outDir, '.gitkeep'), '')
  },
}

export default defineConfig({
  plugins: [preact(), keepGitkeep],
  base: './',
  build: {
    outDir,
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
