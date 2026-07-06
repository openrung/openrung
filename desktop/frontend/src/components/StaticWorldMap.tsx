// Static SVG fallback for the exit-node map, shown when WebGL2 is unavailable
// (see map/webgl.ts). Equirectangular projection of the same ExitNodeRegion
// markers, backed by the bundled country centroids — non-interactive panning,
// but a fully functional, clickable exit picker.
import type { ExitNodeRegion } from '../core/model/exitNode';
import { palette } from '../theme';

interface Props {
  regions: ExitNodeRegion[];
  onSelect: (countryCode: string) => void;
}

const WIDTH = 720;
const HEIGHT = 360;

function project(longitude: number, latitude: number): { x: number; y: number } {
  return {
    x: ((longitude + 180) / 360) * WIDTH,
    y: ((90 - latitude) / 180) * HEIGHT,
  };
}

export function StaticWorldMap({ regions, onSelect }: Props) {
  return (
    <div className="map-root map-fallback">
      <svg viewBox={`0 0 ${WIDTH} ${HEIGHT}`} preserveAspectRatio="xMidYMid meet" role="img" aria-label="Exit node map">
        <rect x={0} y={0} width={WIDTH} height={HEIGHT} fill={palette.screen} />
        {/* graticule every 30° so the projection reads as a map, not a void */}
        {[...Array(11)].map((_, i) => {
          const x = (i / 12) * WIDTH + WIDTH / 24;
          return <line key={`v${i}`} x1={x} y1={0} x2={x} y2={HEIGHT} stroke={palette.borderDim} strokeWidth={0.4} opacity={0.4} />;
        })}
        {[...Array(5)].map((_, i) => {
          const y = ((i + 1) / 6) * HEIGHT;
          return <line key={`h${i}`} x1={0} y1={y} x2={WIDTH} y2={y} stroke={palette.borderDim} strokeWidth={0.4} opacity={0.4} />;
        })}
        {regions.map(region => {
          const { x, y } = project(region.longitude, region.latitude);
          const label = region.city ?? region.countryName;
          return (
            <g
              key={`${region.countryCode}|${region.city ?? ''}`}
              className="static-marker"
              transform={`translate(${x}, ${y})`}
              onClick={() => onSelect(region.countryCode)}
              role="button"
              aria-label={`Connect to ${label}`}
            >
              <circle r={9} fill={palette.terminalGreen} opacity={0.18} />
              <circle r={4.5} fill={palette.terminalGreen} stroke={palette.markerStroke} strokeWidth={1} />
              <text x={8} y={3.5} fill={palette.bodyText} fontSize={9}>
                {label}
                {region.nodeCount > 1 ? ` ×${region.nodeCount}` : ''}
              </text>
            </g>
          );
        })}
      </svg>
    </div>
  );
}
