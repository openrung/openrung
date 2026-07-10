// Opens a URL in the user's real browser. Under Wails, the webview would
// otherwise navigate itself, so we route through the runtime's BrowserOpenURL;
// in a plain browser preview it falls back to window.open.
export function openExternal(url: string): void {
  const runtime = typeof window !== 'undefined' ? window.runtime : undefined;
  if (runtime?.BrowserOpenURL) {
    runtime.BrowserOpenURL(url);
    return;
  }
  window.open(url, '_blank', 'noopener,noreferrer');
}
