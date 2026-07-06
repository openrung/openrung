// Scrollable exit-node location list, rendered over the map (mirrors the RN
// RelayList). Each row is one broker-served location (flag + "City, Country" +
// relay count); clicking a row connects to that country, the same action the
// map markers trigger. The desktop region model has no per-relay children, so
// rows connect to the country directly (Android's collapsed behavior).
import type { ExitNodeRegion } from '../core/model/exitNode';
import type { DirectoryStatus } from '../core/model/exitNode';
import { countryFlag } from './countryFlag';

interface Props {
  regions: ExitNodeRegion[];
  status: DirectoryStatus;
  onSelect: (countryCode: string) => void;
  onRetry: () => void;
}

function relayCountLabel(n: number): string {
  return n === 1 ? '1 relay' : `${n} relays`;
}

export function RelayList({ regions, status, onSelect, onRetry }: Props) {
  if (regions.length === 0) {
    const failed = status === 'failed';
    const msg = failed
      ? 'Directory unavailable — click to retry'
      : status === 'loading'
        ? 'Loading locations…'
        : 'No locations available';
    return (
      <div className="or-list-panel or-list-status">
        <button
          type="button"
          className={`or-list-status-text ${failed ? 'is-failed' : ''}`}
          onClick={failed || status === 'loaded' ? onRetry : undefined}
        >
          {msg}
        </button>
      </div>
    );
  }

  return (
    <div className="or-list-panel">
      <div className="or-list-scroll">
        {regions.map((region, i) => (
          <div key={region.countryCode + (region.city ?? '')}>
            {i > 0 && <div className="or-list-divider" />}
            <button
              type="button"
              className="or-list-row"
              onClick={() => onSelect(region.countryCode)}
            >
              <span className="or-list-flag">{countryFlag(region.countryCode)}</span>
              <span className="or-list-label">
                {region.city ? `${region.city}, ${region.countryName}` : region.countryName}
              </span>
              <span className="or-list-count">{relayCountLabel(region.nodeCount)}</span>
              <span className="or-list-chevron">▸</span>
            </button>
          </div>
        ))}
      </div>
    </div>
  );
}
