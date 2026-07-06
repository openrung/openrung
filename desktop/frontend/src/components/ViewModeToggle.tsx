// MAP/LIST segmented control, mirroring the RN app's ViewModeToggle. Sits
// top-center over the map, below the header.
export type ViewMode = 'map' | 'list';

const MapGlyph = () => (
  <svg
    width="13"
    height="13"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="2.2"
    strokeLinecap="round"
    strokeLinejoin="round"
  >
    <polygon points="1 6 8 3 16 6 23 3 23 18 16 21 8 18 1 21 1 6" />
    <line x1="8" y1="3" x2="8" y2="18" />
    <line x1="16" y1="6" x2="16" y2="21" />
  </svg>
);

const ListGlyph = () => (
  <svg
    width="13"
    height="13"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="2.2"
    strokeLinecap="round"
    strokeLinejoin="round"
  >
    <line x1="8" y1="6" x2="21" y2="6" />
    <line x1="8" y1="12" x2="21" y2="12" />
    <line x1="8" y1="18" x2="21" y2="18" />
    <line x1="3" y1="6" x2="3.01" y2="6" />
    <line x1="3" y1="12" x2="3.01" y2="12" />
    <line x1="3" y1="18" x2="3.01" y2="18" />
  </svg>
);

export function ViewModeToggle({
  mode,
  onChange,
}: {
  mode: ViewMode;
  onChange: (m: ViewMode) => void;
}) {
  return (
    <div className="or-view-toggle">
      <button
        type="button"
        className={`or-seg ${mode === 'map' ? 'is-active' : ''}`}
        onClick={() => onChange('map')}
      >
        <MapGlyph />
        <span className="or-seg-label">MAP</span>
      </button>
      <button
        type="button"
        className={`or-seg ${mode === 'list' ? 'is-active' : ''}`}
        onClick={() => onChange('list')}
      >
        <ListGlyph />
        <span className="or-seg-label">LIST</span>
      </button>
    </div>
  );
}
