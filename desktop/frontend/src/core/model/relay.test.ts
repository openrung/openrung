// Ported from openrung-mobile-app/__tests__/core/relay.test.ts (Jest → vitest;
// the module under test is byte-identical, so the assertions carry over).
import { describe, expect, it } from 'vitest';
import {
  isUsable,
  orderedCandidates,
  selectFirstUsable,
  serverTimeMs,
  type RelayDescriptor,
} from './relay';

const nowMs = Date.parse('2026-07-06T00:00:00Z');

function usableRelay(overrides: Partial<RelayDescriptor> = {}): RelayDescriptor {
  return {
    id: 'r1',
    public_host: '1.2.3.4',
    public_port: 443,
    protocol: 'vless-reality-vision',
    client_id: 'uuid',
    reality_public_key: 'pk',
    short_id: 'sid',
    server_name: 'sni',
    flow: 'xtls-rprx-vision',
    exit_mode: 'direct',
    max_sessions: 8,
    max_mbps: 100,
    volunteer_version: '1',
    registered_at: '2026-07-05T00:00:00Z',
    last_heartbeat_at: '2026-07-06T00:00:00Z',
    expires_at: '2026-07-06T01:00:00Z',
    ...overrides,
  };
}

describe('isUsable', () => {
  it('accepts a well-formed unexpired relay', () => {
    expect(isUsable(usableRelay(), nowMs)).toBe(true);
  });

  it('rejects an expired relay', () => {
    expect(isUsable(usableRelay({ expires_at: '2026-07-05T00:00:00Z' }), nowMs)).toBe(false);
  });

  it('rejects an unparseable expiry', () => {
    expect(isUsable(usableRelay({ expires_at: 'not-a-date' }), nowMs)).toBe(false);
  });

  it('rejects the wrong protocol / flow / exit mode', () => {
    expect(isUsable(usableRelay({ protocol: 'other' }), nowMs)).toBe(false);
    expect(isUsable(usableRelay({ flow: 'other' }), nowMs)).toBe(false);
    expect(isUsable(usableRelay({ exit_mode: 'tunnel' }), nowMs)).toBe(false);
  });

  it('rejects missing connection fields', () => {
    expect(isUsable(usableRelay({ public_host: '' }), nowMs)).toBe(false);
    expect(isUsable(usableRelay({ public_port: 0 }), nowMs)).toBe(false);
    expect(isUsable(usableRelay({ server_name: '' }), nowMs)).toBe(false);
  });
});

describe('orderedCandidates / selectFirstUsable', () => {
  it('filters to usable relays preserving broker order', () => {
    const relays = [
      usableRelay({ id: 'bad', protocol: 'other' }),
      usableRelay({ id: 'a' }),
      usableRelay({ id: 'b' }),
    ];
    expect(orderedCandidates(relays, nowMs).map(r => r.id)).toEqual(['a', 'b']);
    expect(selectFirstUsable(relays, nowMs)?.id).toBe('a');
  });

  it('returns null when nothing is usable', () => {
    expect(selectFirstUsable([usableRelay({ expires_at: 'x' })], nowMs)).toBeNull();
  });
});

describe('serverTimeMs', () => {
  it('parses ISO server time', () => {
    expect(serverTimeMs({ count: 0, server_time: '2026-07-06T00:00:00Z', relays: [] })).toBe(nowMs);
  });

  it('throws on unparseable server time', () => {
    expect(() => serverTimeMs({ count: 0, server_time: 'nope', relays: [] })).toThrow();
  });
});
