// Bundled world geometry for the exit-node map. Sourced from world-atlas
// (Natural Earth, public domain) and shipped in-app rather than fetched from
// demotiles.maplibre.org — discovery runs BEFORE the tunnel is up, so a remote
// tile dependency is both a censorship liability and a privacy leak.
//
// We use the 110m COUNTRIES topology (not just land) so the map shows country
// borders, matching the RN app's `countries` vector layer: a faint green fill
// per country plus a neon-green boundary network of coastlines AND borders.
import { feature, mesh } from 'topojson-client';
import countries110m from 'world-atlas/countries-110m.json';
import type {
  Feature,
  FeatureCollection,
  GeoJsonProperties,
  Geometry,
  MultiLineString,
  MultiPolygon,
  Polygon,
  Position,
} from 'geojson';

// Loosely typed because the JSON import carries no TopoJSON types.
type Topo = Parameters<typeof feature>[0];
type TopoObj = Parameters<typeof feature>[1];
const topology = countries110m as unknown as Topo & { objects: { countries: TopoObj } };

// Unwrap a coordinate sequence so longitudes stay continuous instead of jumping
// ±360 at the antimeridian. Without this, MapLibre tessellates a polygon whose
// ring jumps +180→−180 (Russia's Chukotka, Antarctica, Fiji) into a huge
// triangular wedge across the map, and strokes a line straight across it.
// Making the longitudes continuous (e.g. Chukotka at 190° instead of −170°)
// lets MapLibre fill/stroke it correctly; renderWorldCopies draws the >180 part
// wrapped onto the other side.
function unwrap(coords: Position[]): Position[] {
  if (coords.length === 0) {
    return [];
  }
  const out: Position[] = [[coords[0][0], coords[0][1]]];
  let offset = 0;
  for (let i = 1; i < coords.length; i++) {
    const delta = coords[i][0] - coords[i - 1][0];
    if (delta > 180) {
      offset -= 360;
    } else if (delta < -180) {
      offset += 360;
    }
    out.push([coords[i][0] + offset, coords[i][1]]);
  }
  return out;
}

function unwrapRings(rings: Position[][]): Position[][] {
  return rings.map(unwrap);
}

/** One filled polygon per country (the landmass), with antimeridian-safe rings. */
const rawCountries = feature(topology, topology.objects.countries) as FeatureCollection<
  Geometry,
  GeoJsonProperties
>;

export const countriesFeature: FeatureCollection<Geometry, GeoJsonProperties> = {
  type: 'FeatureCollection',
  features: rawCountries.features.map(f => {
    if (f.geometry.type === 'Polygon') {
      const geometry: Polygon = {
        type: 'Polygon',
        coordinates: unwrapRings(f.geometry.coordinates),
      };
      return { ...f, geometry };
    }
    if (f.geometry.type === 'MultiPolygon') {
      const geometry: MultiPolygon = {
        type: 'MultiPolygon',
        coordinates: f.geometry.coordinates.map(unwrapRings),
      };
      return { ...f, geometry };
    }
    return f;
  }),
};

/** Coastlines + country borders as a single MultiLineString, antimeridian-safe. */
const rawMesh = mesh(
  topology,
  topology.objects.countries as Parameters<typeof mesh>[1],
) as MultiLineString;

export const landOutline: Feature<MultiLineString> = {
  type: 'Feature',
  properties: {},
  geometry: {
    type: 'MultiLineString',
    coordinates: rawMesh.coordinates.map(unwrap),
  },
};
