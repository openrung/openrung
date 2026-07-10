// Human-friendly formatting for the Home screen stats. Pure functions, unit
// tested in format.test.ts.

const BYTE_UNITS = ['KB', 'MB', 'GB', 'TB'] as const;

/** Formats a byte count as "512 B", "1.5 KB", "240 MB", "3.2 GB" (base 1024). */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) {
    return '0 B';
  }
  if (bytes < 1024) {
    return `${Math.round(bytes)} B`;
  }
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024;
    unit += 1;
  }
  const text = value >= 100 ? String(Math.round(value)) : value.toFixed(1);
  return `${text} ${BYTE_UNITS[unit]}`;
}

/** Formats an elapsed duration as "42s", "12m 05s", "3h 04m", "2d 5h". */
export function formatDuration(ms: number): string {
  const totalSeconds = Number.isFinite(ms) && ms > 0 ? Math.floor(ms / 1000) : 0;
  const days = Math.floor(totalSeconds / 86_400);
  const hours = Math.floor((totalSeconds % 86_400) / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  if (days > 0) {
    return `${days}d ${hours}h`;
  }
  if (hours > 0) {
    return `${hours}h ${pad2(minutes)}m`;
  }
  if (minutes > 0) {
    return `${minutes}m ${pad2(seconds)}s`;
  }
  return `${seconds}s`;
}

function pad2(n: number): string {
  return String(n).padStart(2, '0');
}
