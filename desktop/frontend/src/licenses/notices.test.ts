// @vitest-environment node
// Guards the bundled license notices: the in-app GPL text must stay
// byte-identical to the repository's LICENSE (the licenses screen is a GPL
// §6 compliance surface, so silent drift matters), and the component
// inventory must stay well-formed. Runs in the node environment so the
// LICENSE path resolves from this file's location, not the invocation cwd.
/// <reference types="node" />
import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { GPL_TEXT, THIRD_PARTY_TEXT, components } from './notices';

describe('bundled license notices', () => {
  it('GPL_TEXT matches the repository LICENSE byte-for-byte', () => {
    // frontend/src/licenses → repo root is four levels up (desktop/ is a
    // nested module inside the openrung repo).
    const licensePath = fileURLToPath(new URL('../../../../LICENSE', import.meta.url));
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

  it('third-party notices carry complete license texts, not placeholders', () => {
    // The in-app copy is the only notice surface shipped with desktop
    // packages, so the appendix must reproduce the full texts.
    expect(THIRD_PARTY_TEXT).not.toMatch(/full (standard )?(disclaimer|text)? ?as in the standard/i);
    // Every BSD-style disclaimer must run through to its final clause.
    const disclaimers = THIRD_PARTY_TEXT.match(/THIS SOFTWARE IS PROVIDED/g) ?? [];
    for (const _ of disclaimers) {
      expect(THIRD_PARTY_TEXT).toContain('POSSIBILITY OF SUCH DAMAGE');
    }
    // Apache-2.0 applies to shipped code (go-reflector, gVisor); its full
    // text must be present, not a URL pointer.
    expect(THIRD_PARTY_TEXT).toContain('Apache License');
    expect(THIRD_PARTY_TEXT).toContain('END OF TERMS AND CONDITIONS');
  });
});
