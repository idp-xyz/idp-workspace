import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

// The shell is served by idp-workspace itself (same origin as the API, ADR-0001 D9). In development
// `npm run dev -w apps/web` proxies the API to a local idp-workspace.
export default defineConfig({
  plugins: [react()],
  build: { outDir: 'dist', assetsDir: 'assets', sourcemap: false, modulePreload: { polyfill: false } },
  server: {
    proxy: {
      '/v1': { target: 'http://127.0.0.1:8560', changeOrigin: false },
      '/api': { target: 'http://127.0.0.1:8560', changeOrigin: false },
      '/shell-config.json': { target: 'http://127.0.0.1:8560' },
    },
  },
  test: { environment: 'jsdom', include: ['test/**/*.test.{ts,tsx}'] },
});
