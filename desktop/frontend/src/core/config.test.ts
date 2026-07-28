import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import wailsConfig from '../../../wails.json';
import { APP_VERSION } from './config';

describe('APP_VERSION', () => {
  it('comes from canonical desktop/VERSION', () => {
    // vitest runs with desktop/frontend as its root, so ../VERSION is
    // desktop/VERSION.
    const version = readFileSync(resolve(process.cwd(), '../VERSION'), 'utf8').trim();
    expect(APP_VERSION).toBe(version);
  });

  it('matches the wails.json packaging copy', () => {
    // wails.json only carries a copy for the native package metadata;
    // readAppVersion in vite.config.ts refuses to build on drift.
    expect(APP_VERSION).toBe(wailsConfig.info.productVersion);
  });
});
