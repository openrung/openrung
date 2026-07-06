// The "openrung-neon" MapLibre style: black ocean, dark-green land, dim-green
// coastlines — the same flat, country-agnostic look as the mobile map, ported
// to the GL JS style spec. No `glyphs` entry: the map has no symbol/text
// layers (region labels are DOM markers), so no glyph PBFs are needed, which
// is what lets the whole map render from bundled assets with zero network.
import type { StyleSpecification } from 'maplibre-gl';
import { countriesFeature, landOutline } from './worldLand';
import { palette } from '../theme';

export function neonStyle(): StyleSpecification {
  return {
    version: 8,
    name: 'openrung-neon',
    sources: {
      // Filled land uses the country polygons (tessellation wraps the
      // antimeridian cleanly); the boundary stroke (coastlines + country
      // borders) uses a separate MultiLineString with antimeridian-crossing
      // segments removed, so the outline doesn't streak.
      land: { type: 'geojson', data: countriesFeature },
      'land-coastline': { type: 'geojson', data: landOutline },
    },
    layers: [
      {
        id: 'ocean',
        type: 'background',
        paint: { 'background-color': palette.screen },
      },
      {
        id: 'land-fill',
        type: 'fill',
        source: 'land',
        // Faint bright-green landmass (RN uses terminalGreen @ 0.12), not a
        // dark fill — this is half of why the RN map reads brighter.
        paint: {
          'fill-color': palette.terminalGreen,
          'fill-opacity': 0.12,
        },
      },
      {
        id: 'land-outline',
        type: 'line',
        source: 'land-coastline',
        // Crisp bright-green coastline (RN: terminalGreen, 1px, 0.85 opacity).
        paint: {
          'line-color': palette.terminalGreen,
          'line-width': 1,
          'line-opacity': 0.85,
        },
      },
    ],
  };
}
