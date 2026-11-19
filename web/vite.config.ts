import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  plugins: [react()],
  build: {
    // The placeholder beside it is the Go package's and a build never touches it.
    outDir: 'dist/app',
    // The page's policy has no data: source.
    assetsInlineLimit: 0,
  },
  server: {
    // For npm run dev beside a running cavu serve on its default address.
    proxy: { '/api': 'http://127.0.0.1:7117' },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: './src/test-setup.ts',
  },
})
