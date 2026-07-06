// Recently-used exit locations as bordered pills, above the connect card
// (mirrors the RN RecentsSection). Reads state.native.recents, which the Go
// service populates on each successful connect. Renders nothing when empty.
import type { RecentNode } from '../native/types';
import { countryFlag } from './countryFlag';

export function RecentsSection({
  recents,
  onSelect,
}: {
  recents: RecentNode[];
  onSelect: (countryCode: string) => void;
}) {
  if (!recents || recents.length === 0) {
    return null;
  }
  return (
    <div className="or-recents">
      <span className="or-recents-label">RECENTS</span>
      <div className="or-recents-row">
        {recents.map(item => (
          <button
            key={item.countryCode}
            type="button"
            className="or-recents-pill"
            onClick={() => onSelect(item.countryCode)}
          >
            <span className="or-recents-flag">{countryFlag(item.countryCode)}</span>
            <span className="or-recents-pill-label">{item.label}</span>
          </button>
        ))}
      </div>
    </div>
  );
}
