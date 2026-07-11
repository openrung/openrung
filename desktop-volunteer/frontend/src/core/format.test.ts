import { describe, expect, it } from 'vitest';
import { formatBytes, formatDuration } from './format';

describe('formatBytes', () => {
  it('renders sub-KB counts in bytes', () => {
    expect(formatBytes(0)).toBe('0 B');
    expect(formatBytes(512)).toBe('512 B');
    expect(formatBytes(1023)).toBe('1023 B');
  });

  it('scales through KB / MB / GB with one decimal', () => {
    expect(formatBytes(1024)).toBe('1.0 KB');
    expect(formatBytes(1536)).toBe('1.5 KB');
    expect(formatBytes(1024 * 1024)).toBe('1.0 MB');
    expect(formatBytes(5.5 * 1024 * 1024 * 1024)).toBe('5.5 GB');
  });

  it('drops the decimal at 100+ of a unit', () => {
    expect(formatBytes(150 * 1024)).toBe('150 KB');
    expect(formatBytes(999.4 * 1024 * 1024)).toBe('999 MB');
  });

  it('caps the unit at TB', () => {
    expect(formatBytes(2 * 1024 ** 4)).toBe('2.0 TB');
    expect(formatBytes(1024 ** 5)).toBe('1024 TB');
  });

  it('clamps negative and non-finite input to 0 B', () => {
    expect(formatBytes(-5)).toBe('0 B');
    expect(formatBytes(Number.NaN)).toBe('0 B');
    expect(formatBytes(Number.POSITIVE_INFINITY)).toBe('0 B');
  });
});

describe('formatDuration', () => {
  it('renders seconds only under a minute', () => {
    expect(formatDuration(0)).toBe('0s');
    expect(formatDuration(999)).toBe('0s');
    expect(formatDuration(42_000)).toBe('42s');
  });

  it('renders minutes with zero-padded seconds', () => {
    expect(formatDuration(62_000)).toBe('1m 02s');
    expect(formatDuration(754_000)).toBe('12m 34s');
  });

  it('renders hours with zero-padded minutes', () => {
    expect(formatDuration(3_600_000)).toBe('1h 00m');
    expect(formatDuration((3 * 3600 + 4 * 60 + 5) * 1000)).toBe('3h 04m');
  });

  it('renders days with hours', () => {
    expect(formatDuration((2 * 86_400 + 5 * 3600) * 1000)).toBe('2d 5h');
  });

  it('clamps negative and non-finite input to 0s', () => {
    expect(formatDuration(-1000)).toBe('0s');
    expect(formatDuration(Number.NaN)).toBe('0s');
  });
});
