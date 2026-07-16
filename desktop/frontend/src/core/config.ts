/**
 * Desktop app config. Trimmed adaptation of openrung-mobile-app/src/config.ts:
 * on desktop, broker discovery (candidate ordering, failover, 429 backoff)
 * lives in Go (desktop/config + desktop/discovery), so only the values the
 * frontend still reads directly are kept here. Values match the mobile
 * AppConfig so both clients behave identically.
 */
export const AppConfig = {
  /** HTTPS, Cloudflare-fronted discovery endpoint. Displayed; the Go layer owns the real candidate list. */
  DEFAULT_BROKER_URL: 'https://broker.openrung.org/',

  /** Broker max page size for the map directory (matches Go config.DirectoryRelayLimit). */
  DIRECTORY_RELAY_LIMIT: 20,

  /** Most-recently connected locations kept for the "Recents" row. */
  MAX_RECENTS: 8,

  /** GPL-3.0 corresponding-source offer surfaced in the licenses screen. */
  SOURCE_URL: 'https://github.com/openrung/openrung',
} as const;

/** App version shown on the About screen. */
export const APP_VERSION = __APP_VERSION__;
