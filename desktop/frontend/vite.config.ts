// The desktop frontend is the mobile app's React Native UI, compiled for the
// Wails webview with react-native-web. The UI source is not copied here: it is
// the openrung-mobile-app repository, pinned as the desktop/mobile-app
// submodule, so the desktop UI cannot drift from mobile. This package holds
// only the web shims and the bridge to the Go service.
import { existsSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import react from '@vitejs/plugin-react';
import { loadEnv } from 'vite';
import { defineConfig } from 'vitest/config';

// Shared with the packaging scripts, so the frontend and the Go binary cannot
// disagree about the version: reads canonical desktop/VERSION.
import { readAppVersion } from '../scripts/versioned-wails-build.mjs';
import {
  desktopLicenseNotices,
  mobileAssetRequires,
  mobileTypeScript,
  moduleId,
} from './build/mobilePlugins';

const here = path.dirname(fileURLToPath(import.meta.url));

/** A file of this package, as a module id (forward slashes on every platform). */
function local(file: string): string {
  return moduleId(path.join(here, file));
}

/**
 * The mobile app source: the pinned submodule, unless OPENRUNG_MOBILE_APP (in
 * the environment or an untracked `.env.local`) points at another checkout to
 * develop against. Release builds must not set it. Returned as a module id:
 * everything below compares it with, or builds, the ids Vite works in.
 */
function findMobileRoot(mode: string): string {
  const override =
    process.env.OPENRUNG_MOBILE_APP ?? loadEnv(mode, here, 'OPENRUNG_').OPENRUNG_MOBILE_APP;
  const root = override ? path.resolve(here, override) : path.join(here, '../mobile-app');
  if (!existsSync(path.join(root, 'App.tsx'))) {
    throw new Error(
      override
        ? `OPENRUNG_MOBILE_APP=${override} is not an openrung-mobile-app checkout`
        : `${root} is empty; run \`git submodule update --init desktop/mobile-app\``,
    );
  }
  return moduleId(root);
}

// Platform files: `Foo.web.tsx` wins over `Foo.tsx`, as Metro does for native.
const extensions = [
  '.web.tsx',
  '.web.ts',
  '.web.jsx',
  '.web.js',
  '.mjs',
  '.js',
  '.mts',
  '.ts',
  '.jsx',
  '.tsx',
  '.json',
];

// Every bare import the mobile source makes must resolve to THIS package's
// node_modules, never the mobile checkout's (two Reacts break hooks, and the
// mobile tree holds native-only builds).
const sharedDependencies = [
  'react',
  'react-dom',
  'react-native-web',
  'react-native-safe-area-context',
  'react-native-svg',
  '@react-native-async-storage/async-storage',
  '@noble/ed25519',
  '@noble/hashes',
];

export default defineConfig(({ mode }) => {
  const mobileRoot = findMobileRoot(mode);
  return {
    plugins: [
      mobileAssetRequires(mobileRoot),
      desktopLicenseNotices(mobileRoot, local('src/licenses/notices.ts')),
      mobileTypeScript(mobileRoot),
      react(),
    ],
    resolve: {
      extensions,
      dedupe: sharedDependencies,
      alias: [
        { find: /^react-native$/, replacement: local('src/shims/reactNative.ts') },
        // Native-only modules, replaced by web implementations of the same API.
        {
          find: /^@maplibre\/maplibre-react-native$/,
          replacement: local('src/shims/maplibre.tsx'),
        },
        {
          find: /^react-native-bottom-tabs$/,
          replacement: local('src/shims/bottomTabs.tsx'),
        },
        {
          find: /^@react-native\/assets-registry\/registry$/,
          replacement: local('src/shims/assetsRegistry.ts'),
        },
        { find: /^@mobile\//, replacement: `${mobileRoot}/` },
      ],
    },
    // The mobile tree is compiled by mobileTypeScript above.
    esbuild: { exclude: [/\.js$/, `${mobileRoot}/**`] },
    // React Native code reaches for Node-style `global` (react-native-web's
    // Animated does); browsers only have globalThis.
    define: { global: 'globalThis', __APP_VERSION__: JSON.stringify(readAppVersion()) },
    optimizeDeps: {
      esbuildOptions: { resolveExtensions: extensions, define: { global: 'globalThis' } },
    },
    server: {
      port: 5173,
      strictPort: true,
      fs: { allow: [here, mobileRoot] },
    },
    worker: {
      format: 'es',
    },
    build: {
      target: 'es2022',
    },
    test: {
      environment: 'jsdom',
      globals: true,
      include: ['src/**/*.test.ts', 'src/**/*.test.tsx', 'build/**/*.test.ts'],
      server: {
        // Let Vite transform these instead of Node loading them raw: they
        // import `react-native` and rely on the `.web.js` resolution above.
        deps: { inline: [/react-native/, /@react-native/] },
      },
    },
  };
});
