import { readFileSync } from 'node:fs';
import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

interface WailsConfig {
  info?: {
    productVersion?: unknown;
  };
}

const wailsConfig = JSON.parse(
  readFileSync(new URL('../wails.json', import.meta.url), 'utf8'),
) as WailsConfig;
const appVersion = wailsConfig.info?.productVersion;

if (
  typeof appVersion !== 'string' ||
  !/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(appVersion)
) {
  throw new Error('desktop/wails.json info.productVersion must be a semantic X.Y.Z version');
}

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
