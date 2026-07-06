// The exit-node map: MapLibre GL JS port of the mobile @maplibre/maplibre-react-native
// map. Same neon style and region model; markers are DOM elements (maplibregl.Marker)
// rather than a symbol layer, so no glyph atlas is needed and the whole map renders
// from bundled assets. Falls back to a static SVG map when WebGL2 is unavailable.
import { useEffect, useRef, useState } from 'react';
import maplibregl from 'maplibre-gl';
import 'maplibre-gl/dist/maplibre-gl.css';
import type { ExitNodeRegion } from '../core/model/exitNode';
import { neonStyle } from '../map/neonStyle';
import { hasWebGL2 } from '../map/webgl';
import { StaticWorldMap } from './StaticWorldMap';

interface Props {
  regions: ExitNodeRegion[];
  onSelect: (countryCode: string) => void;
}

function markerLabel(region: ExitNodeRegion): string {
  // Place name only; the relay count is shown as a separate badge on the dot.
  return region.city ?? region.countryName;
}

function buildMarkerElement(region: ExitNodeRegion, onClick: () => void): HTMLElement {
  const el = document.createElement('button');
  el.type = 'button';
  el.className = 'map-marker';
  // Core dot carries the neon bloom (via box-shadow rings in CSS); the count
  // badge and place label are separate elements, matching the RN marker.
  const dot = document.createElement('span');
  dot.className = 'map-marker-dot';
  if (region.nodeCount > 1) {
    const count = document.createElement('span');
    count.className = 'map-marker-count';
    count.textContent = String(region.nodeCount);
    dot.append(count);
  }
  const label = document.createElement('span');
  label.className = 'map-marker-label';
  // textContent (not innerHTML): region names come from the broker and must not
  // be able to inject markup.
  label.textContent = markerLabel(region);
  el.append(dot, label);
  el.addEventListener('click', onClick);
  return el;
}

export function ExitNodeMap({ regions, onSelect }: Props) {
  const containerRef = useRef<HTMLDivElement>(null);
  const mapRef = useRef<maplibregl.Map | null>(null);
  const markersRef = useRef<maplibregl.Marker[]>([]);
  // onSelect can change identity per render; read it through a ref so markers
  // don't need rebuilding when only the handler changes.
  const onSelectRef = useRef(onSelect);
  onSelectRef.current = onSelect;
  const [webglOk] = useState(hasWebGL2);
  // mapReady flips once the map object exists. DOM markers only need the map's
  // projection (set from center/zoom in the constructor), NOT the first GL frame
  // — so we deliberately do NOT wait for the 'load' event, which can stall under
  // software WebGL and would otherwise leave the map marker-less.
  const [mapReady, setMapReady] = useState(false);

  useEffect(() => {
    if (!webglOk || containerRef.current == null) {
      return;
    }
    const map = new maplibregl.Map({
      container: containerRef.current,
      style: neonStyle(),
      center: [116, 18], // Asia-Pacific overview, matching the mobile app
      zoom: 2.2,
      minZoom: 1.2,
      maxZoom: 4.8,
      attributionControl: false,
      dragRotate: false,
      pitchWithRotate: false,
    });
    mapRef.current = map;
    setMapReady(true);
    return () => {
      setMapReady(false);
      map.remove();
      mapRef.current = null;
    };
  }, [webglOk]);

  useEffect(() => {
    const map = mapRef.current;
    if (!webglOk || map == null || !mapReady) {
      return;
    }
    markersRef.current = regions.map(region =>
      new maplibregl.Marker({
        element: buildMarkerElement(region, () => onSelectRef.current(region.countryCode)),
        anchor: 'center',
      })
        .setLngLat([region.longitude, region.latitude])
        .addTo(map),
    );
    return () => {
      for (const marker of markersRef.current) {
        marker.remove();
      }
      markersRef.current = [];
    };
  }, [regions, webglOk, mapReady]);

  if (!webglOk) {
    return <StaticWorldMap regions={regions} onSelect={onSelect} />;
  }
  return <div ref={containerRef} className="map-root" />;
}
