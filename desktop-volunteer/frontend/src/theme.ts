/**
 * PORTED from openrung-mobile-app/src/theme.ts. The hex palette is byte-for-byte
 * identical to the mobile app (and its production Compose UI). Desktop changes:
 *   - drop the react-native Platform import; monoFont is a CSS font stack.
 *   - installThemeVariables() publishes every token as a :root CSS custom
 *     property so plain CSS and inline styles share one source of truth, and
 *     the MapLibre "openrung-neon" style reads the same colours.
 */
export const palette = {
  screen: '#030604',
  panel: '#07110B',
  borderDim: '#294F35',
  terminalGreen: '#65F58A',
  bodyText: '#D8FFE0',
  dimText: '#7DA989',
  relayLine: '#A5F2B5',
  connectedButton: '#B6F579',
  onGreenText: '#061008',
  consoleError: '#FFA0A0',
  chipFailedText: '#FFC0C0',
  fabBackground: '#0D1C12',
  fabContent: '#65F58A',
  markerStroke: '#04140A',
} as const;

/** CSS monospace stack (mobile uses Menlo on iOS, monospace on Android). */
export const monoFont = "'Menlo', 'SF Mono', 'Consolas', 'DejaVu Sans Mono', monospace";

export const tokens = {
  glass: 'rgba(7, 17, 11, 0.86)',
  glassDense: 'rgba(3, 6, 4, 0.92)',
  glassBorder: 'rgba(41, 79, 53, 0.9)',
  glow: 'rgba(101, 245, 138, 0.55)',
  glowSoft: 'rgba(101, 245, 138, 0.25)',
  working: '#EAF565',
  radiusSm: '10px',
  radiusMd: '16px',
  radiusLg: '22px',
  edge: '20px',
  // Added for RN visual parity (map chip bg, active toggle tint, outlined-button
  // fill, and the two concentric marker bloom rings).
  chipBackground: '#07110BCC',
  segmentActive: 'rgba(101, 245, 138, 0.12)',
  buttonTint: 'rgba(101, 245, 138, 0.08)',
  markerHalo: 'rgba(101, 245, 138, 0.18)',
  markerHaloOuter: 'rgba(101, 245, 138, 0.07)',
} as const;

/** Publishes palette + tokens as :root CSS variables (--or-<name>). */
export function installThemeVariables(): void {
  const root = document.documentElement;
  for (const [name, value] of Object.entries(palette)) {
    root.style.setProperty(cssVarName(name), value);
  }
  for (const [name, value] of Object.entries(tokens)) {
    root.style.setProperty(cssVarName(name), String(value));
  }
  root.style.setProperty('--or-mono', monoFont);
}

function cssVarName(name: string): string {
  // camelCase → --or-kebab-case
  return '--or-' + name.replace(/[A-Z]/g, m => '-' + m.toLowerCase());
}
