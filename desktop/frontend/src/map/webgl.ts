// WebGL2 capability probe. On Linux, Wails renders through WebKitGTK, whose
// WebGL support varies by GPU/driver (and which we further harden with
// WEBKIT_DISABLE_DMABUF_RENDERER in main.go). When WebGL2 is unavailable the UI
// falls back to a static SVG map so picking an exit node never depends on a GPU.
export function hasWebGL2(): boolean {
  try {
    const canvas = document.createElement('canvas');
    return canvas.getContext('webgl2') != null;
  } catch {
    return false;
  }
}
