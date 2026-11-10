// The Vite plugins that adapt the mobile app's source tree to this build.
//
// Vite hands every hook a module id with forward slashes on every platform,
// while node:path produces backslashes on Windows. Each path these plugins
// compare against an id therefore goes through moduleId() first.
import { transform } from 'esbuild';
import type { Plugin } from 'vite';

import { rewriteAssetRequires } from './assetRequires';

/** A filesystem path in the form Vite uses for module ids: forward slashes. */
export function moduleId(file: string): string {
  return file.replace(/\\/g, '/');
}

function isUnder(root: string, file: string): boolean {
  return file.startsWith(`${root}/`);
}

/** Applies the asset-require rewrite to the mobile source (see assetRequires.ts). */
export function mobileAssetRequires(mobileRoot: string): Plugin {
  const root = moduleId(mobileRoot);
  return {
    name: 'openrung-mobile-asset-requires',
    enforce: 'pre',
    transform(code, id) {
      if (!isUnder(root, id) || !code.includes('require(')) {
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
 * is excluded from Vite's transform (`esbuild.exclude` in vite.config.ts) and
 * compiled here, with the two settings its code needs.
 */
export function mobileTypeScript(mobileRoot: string): Plugin {
  const root = moduleId(mobileRoot);
  return {
    name: 'openrung-mobile-typescript',
    enforce: 'pre',
    async transform(code, id) {
      const file = id.split('?')[0];
      if (!isUnder(root, file) || !/\.tsx?$/.test(file)) {
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
export function desktopLicenseNotices(mobileRoot: string, desktopNoticesFile: string): Plugin {
  const mobileNotices = `${moduleId(mobileRoot)}/src/licenses/notices.ts`;
  const desktopNotices = moduleId(desktopNoticesFile);
  return {
    name: 'openrung-desktop-license-notices',
    enforce: 'pre',
    async resolveId(source, importer, options) {
      if (importer == null || !source.endsWith('/notices')) {
        return null;
      }
      const resolved = await this.resolve(source, importer, { ...options, skipSelf: true });
      return resolved != null && moduleId(resolved.id) === mobileNotices ? desktopNotices : null;
    },
  };
}
