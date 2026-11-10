// @vitest-environment node
import { describe, expect, it } from 'vitest';

import { desktopLicenseNotices, mobileAssetRequires, mobileTypeScript } from './mobilePlugins';

type Hook = (this: unknown, ...args: unknown[]) => unknown;

function hook(plugin: object, name: 'transform' | 'resolveId'): Hook {
  return (plugin as Record<string, Hook>)[name];
}

// The same checkout as node:path names it on each platform, and as Vite names
// its modules on both: always with forward slashes.
const roots = {
  posix: { mobile: '/repo/desktop/mobile-app', frontend: '/repo/desktop/frontend' },
  win32: { mobile: 'C:\\repo\\desktop\\mobile-app', frontend: 'C:\\repo\\desktop\\frontend' },
};
const ids = {
  posix: { mobile: '/repo/desktop/mobile-app', frontend: '/repo/desktop/frontend' },
  win32: { mobile: 'C:/repo/desktop/mobile-app', frontend: 'C:/repo/desktop/frontend' },
};

describe.each(['posix', 'win32'] as const)('mobile source plugins with %s paths', platform => {
  const root = roots[platform];
  const id = ids[platform];
  const separator = platform === 'win32' ? '\\' : '/';
  const desktopNoticesFile = [root.frontend, 'src', 'licenses', 'notices.ts'].join(separator);

  it('compiles TypeScript in the mobile tree', async () => {
    const transform = hook(mobileTypeScript(root.mobile), 'transform');
    const compiled = (await transform.call(
      null,
      'export const App = (): JSX.Element => <view />;',
      `${id.mobile}/App.tsx`,
    )) as { code: string } | null;
    expect(compiled?.code).toContain('react/jsx-runtime');
    expect(compiled?.code).not.toContain('JSX.Element');
  });

  it('compiles a mobile module requested with a query', async () => {
    const transform = hook(mobileTypeScript(root.mobile), 'transform');
    const compiled = (await transform.call(
      null,
      'export const n: number = 1;',
      `${id.mobile}/src/config.ts?v=1`,
    )) as { code: string } | null;
    expect(compiled?.code).toContain('const n = 1');
  });

  it('leaves TypeScript outside the mobile tree to Vite', async () => {
    const transform = hook(mobileTypeScript(root.mobile), 'transform');
    expect(await transform.call(null, 'export {};', `${id.frontend}/src/main.tsx`)).toBeNull();
    // A sibling whose name merely starts with the mobile root.
    expect(await transform.call(null, 'export {};', `${id.mobile}-old/App.tsx`)).toBeNull();
  });

  it('rewrites asset requires in the mobile tree only', () => {
    const transform = hook(mobileAssetRequires(root.mobile), 'transform');
    const source = "const LOGO = require('./logo.png');";
    const rewritten = transform.call(null, source, `${id.mobile}/src/Logo.tsx`) as {
      code: string;
    } | null;
    expect(rewritten?.code).toContain('import __mobileAsset0 from "./logo.png";');
    expect(transform.call(null, source, `${id.frontend}/src/Logo.tsx`)).toBeNull();
  });

  it('swaps the mobile license inventory for the desktop one', async () => {
    const resolveId = hook(desktopLicenseNotices(root.mobile, desktopNoticesFile), 'resolveId');
    const resolvesTo = (resolved: string) =>
      resolveId.call(
        { resolve: async () => ({ id: resolved }) },
        '../licenses/notices',
        `${id.mobile}/src/screens/LicensesScreen.tsx`,
        {},
      );

    expect(await resolvesTo(`${id.mobile}/src/licenses/notices.ts`)).toBe(
      `${id.frontend}/src/licenses/notices.ts`,
    );
    // Any other module that happens to be called notices is left alone.
    expect(await resolvesTo(`${id.mobile}/src/other/notices.ts`)).toBeNull();
  });
});
