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

  it('covers the Windows driver binary embedded in the app binary', () => {
    // The Windows app binary embeds wintun.dll through the linked sing-box
    // engine (verified by byte content; THIRD_PARTY_NOTICES.md §8). Its
    // license text must reach recipients: the component list names it and the
    // full-text screen reproduces the license verbatim.
    const names = components.map(c => c.name);
    expect(names.some(n => n.includes('wintun.dll'))).toBe(true);
    expect(THIRD_PARTY_TEXT).toContain('Wintun Prebuilt Binaries License');
    // The verbatim Wintun license, not just its name (§8 of the license
    // requires its terms to accompany the binary).
    expect(THIRD_PARTY_TEXT).toContain('RESERVATION OF RIGHTS');
  });

  it('says WinDivert is excluded rather than claiming to ship it', () => {
    // Every OpenRung build passes with_external_windivert, so the LGPLv3
    // driver is not in the package and its text is deliberately absent — the
    // packaging gate (desktop/scripts/verify-bundled-engine.mjs) fails a
    // build whose bytes contain it. If that ever changes, this screen must
    // carry the LGPL-3.0 text again, and this test is where it is written
    // down.
    expect(THIRD_PARTY_TEXT).toContain('with_external_windivert');
    expect(THIRD_PARTY_TEXT).not.toContain('GNU LESSER GENERAL PUBLIC LICENSE');
    expect(components.map(c => c.name).some(n => n.includes('WinDivert'))).toBe(false);
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
