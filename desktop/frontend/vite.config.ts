import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';
// Shared with the packaging scripts, so the frontend and the Go binary cannot
// disagree about what counts as a valid version or where it comes from. Reads
// and validates desktop/wails.json, throwing if info.productVersion is missing
// or not a strict X.Y.Z.
import { readProductVersion } from '../scripts/versioned-wails-build.mjs';

const appVersion: string = readProductVersion();

export default defineConfig({
  plugins: [react()],
  define: {
    __APP_VERSION__: JSON.stringify(appVersion),
  },
  server: {
    port: 5173,
    strictPort: true,
  },
  build: {
    target: 'es2022',
  },
  test: {
    environment: 'jsdom',
    globals: true,
    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
  },
});
