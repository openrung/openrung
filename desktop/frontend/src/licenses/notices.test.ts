// Guards the bundled license notices: the in-app GPL text must stay
// byte-identical to the repository's LICENSE (the licenses screen is a GPL
// §6 compliance surface, so silent drift matters), and the component
// inventory must stay well-formed.
/// <reference types="node" />
import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { GPL_TEXT, THIRD_PARTY_TEXT, components } from './notices';

describe('bundled license notices', () => {
  it('GPL_TEXT matches the repository LICENSE byte-for-byte', () => {
    // vitest runs from desktop/frontend; the repo root (where LICENSE lives)
    // is two levels up (desktop/ is a nested module inside the openrung repo).
    const licensePath = resolve(process.cwd(), '../../LICENSE');
    const license = readFileSync(licensePath, 'utf8').replace(/\n$/, '');
    expect(GPL_TEXT).toBe(license);
  });

  it('lists the GPL engine and the frontend stack', () => {
    const names = components.map(c => c.name);
    expect(names.some(n => n.includes('sing-box'))).toBe(true);
    expect(names.some(n => n.includes('React'))).toBe(true);
    expect(names.some(n => n.includes('MapLibre'))).toBe(true);
  });

  it('every component row is complete', () => {
    for (const c of components) {
      expect(c.name).toBeTruthy();
      expect(c.license).toBeTruthy();
      expect(c.url).toMatch(/^https:\/\//);
    }
  });

  it('third-party notices carry the sing-box §7 additional term', () => {
    expect(THIRD_PARTY_TEXT).toContain('no derivative');
    expect(THIRD_PARTY_TEXT).toContain('GPL-3.0-or-later');
  });
});
