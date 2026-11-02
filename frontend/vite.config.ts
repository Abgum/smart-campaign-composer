import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'

// In development the API runs on :8080 (docker compose up api); the dev
// server proxies to it, so the UI uses the same relative URLs as in the
// built image, where the API serves the UI itself.
export default defineConfig({
  plugins: [vue()],
  server: {
    proxy: {
      '/api': { target: 'http://localhost:8080', changeOrigin: false },
      '/readyz': 'http://localhost:8080',
    },
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.spec.ts'],
  },
})
