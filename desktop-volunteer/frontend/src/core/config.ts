/**
 * Volunteer app config. Trimmed from desktop/frontend/src/core/config.ts: the
 * volunteer app's operational settings (broker URL, ports, capacity) live in
 * the Go service and are edited through the Settings screen, so only the
 * static links and the version string remain here.
 */
export const AppConfig = {
  /** Project website surfaced on the About screen and in the consent gate. */
  WEBSITE_URL: 'https://openrung.org',

  /** GPL-3.0 corresponding-source offer surfaced on the About screen. */
  SOURCE_URL: 'https://github.com/openrung/openrung',

  /** Bundled relay engine's upstream source (MPL-2.0 notice on About). */
  XRAY_SOURCE_URL: 'https://github.com/XTLS/Xray-core',
} as const;

/** App version shown on the About screen. */
export const APP_VERSION = '0.1.0';
