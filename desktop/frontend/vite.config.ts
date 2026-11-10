// The desktop frontend is the mobile app's React Native UI, compiled for the
// Wails webview with react-native-web. The UI source is not copied here: it is
// the openrung-mobile-app repository, pinned as the desktop/mobile-app
// submodule, so the desktop UI cannot drift from mobile. This package holds
// only the web shims and the bridge to the Go service.
import { existsSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import react from '@vitejs/plugin-react';
import { transform } from 'esbuild';
import { loadEnv, type Plugin } from 'vite';
import { defineConfig } from 'vitest/config';

// Shared with the packaging scripts, so the frontend and the Go binary cannot
// disagree about the version: reads canonical desktop/VERSION.
import { readAppVersion } from '../scripts/versioned-wails-build.mjs';
import { rewriteAssetRequires } from './build/assetRequires';

const here = path.dirname(fileURLToPath(import.meta.url));

/**
 * The mobile app source: the pinned submodule, unless OPENRUNG_MOBILE_APP (in
 * the environment or an untracked `.env.local`) points at another checkout to
 * develop against. Release builds must not set it.
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
  return root;
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

/** Applies the asset-require rewrite to the mobile source (see build/assetRequires.ts). */
function mobileAssetRequires(root: string): Plugin {
  return {
    name: 'openrung-mobile-asset-requires',
    enforce: 'pre',
    transform(code, id) {
      if (!id.startsWith(root) || !code.includes('require(')) {
        return null;
      }
      const rewritten = rewriteAssetRequires(code);
      return rewritten == null ? null : { code: rewritten, map: null };
    },
  };
}

/**
 * Compiles the mobile TypeScript. Vite's own TypeScript transform insists on
 * loading the nearest tsconfig.json, and the mobile app's extends a React
 * Native preset that is installed only in that repository. So the mobile tree
 * is excluded from Vite's transform (`esbuild.exclude` below) and compiled
 * here, with the two settings its code needs.
 */
function mobileTypeScript(root: string): Plugin {
  return {
    name: 'openrung-mobile-typescript',
    enforce: 'pre',
    async transform(code, id) {
      const file = id.split('?')[0];
      if (!file.startsWith(root) || !/\.tsx?$/.test(file)) {
        return null;
      }
      const result = await transform(code, {
        loader: file.endsWith('x') ? 'tsx' : 'ts',
        jsx: 'automatic',
        target: 'es2022',
        sourcemap: true,
        sourcefile: id,
        tsconfigRaw: { compilerOptions: { useDefineForClassFields: true } },
      });
      return { code: result.code, map: result.map };
    },
  };
}

/**
 * The one mobile module the desktop build replaces rather than shims: the
 * license inventory. It describes what a build ships, and this build ships
 * the desktop binary and this bundle, not the mobile app's native stack.
 */
function desktopLicenseNotices(root: string): Plugin {
  const mobileNotices = path.join(root, 'src/licenses/notices.ts');
  const desktopNotices = path.join(here, 'src/licenses/notices.ts');
  return {
    name: 'openrung-desktop-license-notices',
    enforce: 'pre',
    async resolveId(source, importer, options) {
      if (importer == null || !source.endsWith('/notices')) {
        return null;
      }
      const resolved = await this.resolve(source, importer, { ...options, skipSelf: true });
      return resolved?.id === mobileNotices ? desktopNotices : null;
    },
  };
}

export default defineConfig(({ mode }) => {
  const mobileRoot = findMobileRoot(mode);
  return {
    plugins: [
      mobileAssetRequires(mobileRoot),
      desktopLicenseNotices(mobileRoot),
      mobileTypeScript(mobileRoot),
      react(),
    ],
    resolve: {
      extensions,
      dedupe: sharedDependencies,
      alias: [
        { find: /^react-native$/, replacement: path.join(here, 'src/shims/reactNative.ts') },
        // Native-only modules, replaced by web implementations of the same API.
        {
          find: /^@maplibre\/maplibre-react-native$/,
          replacement: path.join(here, 'src/shims/maplibre.tsx'),
        },
        {
          find: /^react-native-bottom-tabs$/,
          replacement: path.join(here, 'src/shims/bottomTabs.tsx'),
        },
        {
          find: /^@react-native\/assets-registry\/registry$/,
          replacement: path.join(here, 'src/shims/assetsRegistry.ts'),
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
