// Web implementation of the slice of @maplibre/maplibre-react-native that the
// mobile app uses (Map, Camera, GeoJSONSource, Layer, Marker), on maplibre-gl.
// Both libraries take the same MapLibre style spec, so the mobile map style,
// layer paint and layout objects pass through untouched.
//
// Layer paint/layout and the map style are applied once: the mobile app passes
// module-level constants for all of them.
import type { GeoJSON } from 'geojson';
import {
  GeoJSONSource as GlGeoJSONSource,
  Map as GlMap,
  Marker as GlMarker,
  setWorkerUrl,
  type LayerSpecification,
  type MapGeoJSONFeature,
  type MapMouseEvent,
  type PositionAnchor,
  type StyleSpecification,
} from 'maplibre-gl';
// maplibre-gl locates its worker relative to its own module URL at runtime,
// which a bundler cannot see; have Vite bundle the worker and pass its URL.
import workerUrl from 'maplibre-gl/dist/maplibre-gl-worker.mjs?worker&url';
import {
  createContext,
  useContext,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type MutableRefObject,
  type ReactNode,
} from 'react';
import { createPortal } from 'react-dom';
import { StyleSheet, View } from 'react-native-web';

setWorkerUrl(workerUrl);

export type { StyleSpecification };

export interface PressEventWithFeatures {
  features: MapGeoJSONFeature[];
}

interface CameraSettings {
  center?: [number, number];
  zoom?: number;
  minZoom?: number;
  maxZoom?: number;
}

interface MapContextValue {
  /** Null until the style has loaded and sources/layers can be added. */
  map: GlMap | null;
  /** Written by <Camera> before the map is created (child effects run first). */
  camera: MutableRefObject<CameraSettings>;
}

const MapContext = createContext<MapContextValue | null>(null);
const SourceContext = createContext<string | null>(null);

function useMapContext(): MapContextValue {
  const value = useContext(MapContext);
  if (value == null) {
    throw new Error('MapLibre components must be rendered inside <Map>');
  }
  return value;
}

/** Cleanup can run after the map itself was removed; that is not an error. */
function quietly(action: () => void): void {
  try {
    action();
  } catch {
    // The map is already gone, taking its sources, layers and markers with it.
  }
}

interface MapProps {
  style?: unknown;
  mapStyle: StyleSpecification;
  accessibilityLabel?: string;
  children?: ReactNode;
  // Native gesture/ornament props are accepted and ignored.
  [nativeOnlyProp: string]: unknown;
}

function MapView({ style, mapStyle, accessibilityLabel, children }: MapProps) {
  const container = useRef<HTMLDivElement | null>(null);
  const camera = useRef<CameraSettings>({});
  const [map, setMap] = useState<GlMap | null>(null);

  useEffect(() => {
    if (container.current == null) {
      return;
    }
    let instance: GlMap;
    try {
      instance = new GlMap({
        container: container.current,
        style: mapStyle,
        ...camera.current,
        attributionControl: false,
        // Pan + zoom only, matching the mobile map (rotate/tilt stay off).
        dragRotate: false,
        pitchWithRotate: false,
        touchPitch: false,
      });
    } catch (error) {
      // No WebGL in this webview (some Linux GPU/driver combinations). The
      // map stays empty; the relay list view does not depend on it.
      console.warn('OpenRung: the map is unavailable in this webview', error);
      return;
    }
    instance.touchZoomRotate.disableRotation();
    instance.once('load', () => setMap(instance));
    return () => {
      setMap(null);
      instance.remove();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- created once; see file header
  }, []);

  return (
    <View style={style} accessibilityLabel={accessibilityLabel}>
      <div ref={container} style={{ position: 'absolute', inset: 0 }} />
      <MapContext.Provider value={{ map, camera }}>{children}</MapContext.Provider>
    </View>
  );
}

export { MapView as Map };

interface CameraProps {
  initialViewState?: { center?: [number, number]; zoom?: number };
  minZoom?: number;
  maxZoom?: number;
}

export function Camera({ initialViewState, minZoom, maxZoom }: CameraProps): null {
  const { camera } = useMapContext();
  // Layout effect: guaranteed to run before <Map>'s passive effect creates the map.
  useLayoutEffect(() => {
    camera.current = { ...initialViewState, minZoom, maxZoom };
  }, [camera, initialViewState, minZoom, maxZoom]);
  return null;
}

interface Hitbox {
  top: number;
  right: number;
  bottom: number;
  left: number;
}

interface GeoJSONSourceProps {
  id: string;
  data: GeoJSON;
  onPress?: (event: { nativeEvent: PressEventWithFeatures }) => void;
  hitbox?: Hitbox;
  children?: ReactNode;
}

export function GeoJSONSource({ id, data, onPress, hitbox, children }: GeoJSONSourceProps) {
  const { map } = useMapContext();
  const [ready, setReady] = useState(false);
  const latestData = useRef(data);
  latestData.current = data;
  const latestOnPress = useRef(onPress);
  latestOnPress.current = onPress;

  useEffect(() => {
    if (map == null) {
      return;
    }
    map.addSource(id, { type: 'geojson', data: latestData.current });
    setReady(true);
    return () => {
      setReady(false);
      quietly(() => {
        for (const layer of map.getStyle().layers) {
          if ('source' in layer && layer.source === id) {
            map.removeLayer(layer.id);
          }
        }
        map.removeSource(id);
      });
    };
  }, [map, id]);

  useEffect(() => {
    if (map == null || !ready) {
      return;
    }
    const source = map.getSource(id);
    if (source instanceof GlGeoJSONSource) {
      source.setData(data);
    }
  }, [map, ready, id, data]);

  const { top = 0, right = 0, bottom = 0, left = 0 } = hitbox ?? {};
  useEffect(() => {
    if (map == null || !ready) {
      return;
    }
    const featuresAt = (event: MapMouseEvent): MapGeoJSONFeature[] => {
      const layers = map
        .getStyle()
        .layers.filter(layer => 'source' in layer && layer.source === id)
        .map(layer => layer.id);
      if (layers.length === 0) {
        return [];
      }
      const { x, y } = event.point;
      return map.queryRenderedFeatures(
        [
          [x - left, y - top],
          [x + right, y + bottom],
        ],
        { layers },
      );
    };
    const onClick = (event: MapMouseEvent) => {
      const features = featuresAt(event);
      if (features.length > 0) {
        latestOnPress.current?.({ nativeEvent: { features } });
      }
    };
    // Desktop affordance the touch UI never needed: a pointer over tappable markers.
    const onMove = (event: MapMouseEvent) => {
      map.getCanvas().style.cursor = featuresAt(event).length > 0 ? 'pointer' : '';
    };
    map.on('click', onClick);
    map.on('mousemove', onMove);
    return () => {
      map.off('click', onClick);
      map.off('mousemove', onMove);
    };
  }, [map, ready, id, top, right, bottom, left]);

  return <SourceContext.Provider value={ready ? id : null}>{ready ? children : null}</SourceContext.Provider>;
}

interface LayerProps {
  id: string;
  type: 'circle' | 'symbol' | 'fill' | 'line';
  paint?: Record<string, unknown>;
  layout?: Record<string, unknown>;
}

export function Layer({ id, type, paint, layout }: LayerProps): null {
  const { map } = useMapContext();
  const source = useContext(SourceContext);

  useEffect(() => {
    if (map == null || source == null) {
      return;
    }
    map.addLayer({
      id,
      type,
      source,
      ...(paint != null ? { paint } : null),
      ...(layout != null ? { layout } : null),
    } as LayerSpecification);
    return () => {
      quietly(() => {
        if (map.getLayer(id) != null) {
          map.removeLayer(id);
        }
      });
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- paint/layout are constants; see file header
  }, [map, source, id, type]);

  return null;
}

interface MarkerProps {
  lngLat: [number, number];
  anchor?: PositionAnchor;
  pointerEvents?: 'none' | 'auto' | 'box-none' | 'box-only';
  children?: ReactNode;
}

/** A map-anchored React subtree: the children render into a maplibre-gl marker element. */
export function Marker({ lngLat, anchor = 'center', pointerEvents, children }: MarkerProps) {
  const { map } = useMapContext();
  const [element] = useState(() => document.createElement('div'));
  const [longitude, latitude] = lngLat;

  useEffect(() => {
    if (map == null) {
      return;
    }
    element.style.pointerEvents = pointerEvents === 'none' ? 'none' : '';
    const marker = new GlMarker({ element, anchor })
      .setLngLat([longitude, latitude])
      .addTo(map);
    return () => {
      quietly(() => marker.remove());
    };
  }, [map, element, anchor, pointerEvents, longitude, latitude]);

  return createPortal(<View style={styles.marker}>{children}</View>, element);
}

const styles = StyleSheet.create({
  // The marker element is a bare <div>; give the React Native subtree a flex root.
  marker: {
    alignItems: 'flex-start',
  },
});
