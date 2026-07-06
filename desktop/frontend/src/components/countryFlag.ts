// PORTED VERBATIM from openrung-mobile-app/src/components/countryFlag.ts.
// ISO 3166-1 alpha-2 → flag emoji via Unicode regional indicator symbols; a
// neutral flag for anything that isn't a two-letter code.
export function countryFlag(code: string): string {
  const upper = code.trim().toUpperCase();
  if (!/^[A-Z]{2}$/.test(upper)) {
    return '🏳';
  }
  const first = 0x1f1e6 + (upper.charCodeAt(0) - 65);
  const second = 0x1f1e6 + (upper.charCodeAt(1) - 65);
  return String.fromCodePoint(first) + String.fromCodePoint(second);
}
