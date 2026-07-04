package com.openrung.client.ui

import android.graphics.RectF
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.viewinterop.AndroidView
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import androidx.lifecycle.compose.LocalLifecycleOwner
import com.openrung.client.config.AppConfig
import com.openrung.client.model.ExitNodeRegion
import org.maplibre.android.MapLibre
import org.maplibre.android.camera.CameraUpdateFactory
import org.maplibre.android.geometry.LatLng
import org.maplibre.android.maps.MapLibreMapOptions
import org.maplibre.android.maps.MapView
import org.maplibre.android.maps.Style
import org.maplibre.android.style.layers.CircleLayer
import org.maplibre.android.style.layers.PropertyFactory
import org.maplibre.android.style.layers.SymbolLayer
import org.maplibre.android.style.sources.GeoJsonSource
import org.maplibre.geojson.Feature
import org.maplibre.geojson.FeatureCollection
import org.maplibre.geojson.Point

private const val NODE_SOURCE = "openrung-exit-nodes"
private const val NODE_HALO_LAYER = "openrung-exit-nodes-halo"
private const val NODE_CORE_LAYER = "openrung-exit-nodes-core"
private const val NODE_COUNT_LAYER = "openrung-exit-nodes-count"

private const val NODE_GREEN = "#65F58A"
private const val NODE_STROKE = "#04140A"

// Dark neon basemap palette: black ocean, faintly-shaded green land, neon-green borders. Matches
// the app's terminal-green theme; the green markers/halos glow against the black ocean.
private const val OCEAN_COLOR = "#030604"        // app backdrop, so the map blends edge-to-edge
private const val LAND_COLOR = "#65F58A"         // brand green, drawn translucent (see opacity)
private const val LAND_FILL_OPACITY = 0.12
private const val LAND_OUTLINE_COLOR = "#65F58A" // neon-green coastlines / country borders

// Asia-Pacific overview the map opens to (centred over the East/South-East Asian seas).
private val ASIA_PACIFIC_CENTER = LatLng(18.0, 116.0)
private const val ASIA_PACIFIC_ZOOM = 2.2

/**
 * A minimal dark neon style around the MapLibre demo vector tiles: black ocean, faintly-shaded green
 * land, and neon-green coastlines/borders — deliberately omitting the demo style's per-country
 * colours, graticule, and place labels. The node markers are layered on top at runtime.
 */
private fun cleanMapStyleJson(): String =
    """
    {
      "version": 8,
      "name": "openrung-neon",
      "glyphs": "${AppConfig.MAP_GLYPHS_URL}",
      "sources": {
        "maplibre": { "type": "vector", "url": "${AppConfig.MAP_TILES_URL}" }
      },
      "layers": [
        { "id": "ocean", "type": "background", "paint": { "background-color": "$OCEAN_COLOR" } },
        { "id": "land", "type": "fill", "source": "maplibre", "source-layer": "countries",
          "paint": { "fill-color": "$LAND_COLOR", "fill-opacity": $LAND_FILL_OPACITY } },
        { "id": "land-outline", "type": "line", "source": "maplibre", "source-layer": "countries",
          "paint": { "line-color": "$LAND_OUTLINE_COLOR", "line-width": 1.0, "line-opacity": 0.85 } }
      ]
    }
    """.trimIndent()

/**
 * MapLibre-backed map of available volunteer exit nodes. One marker is drawn per [regions] entry
 * (one country/region), using OSS vector tiles — no Play Services or API key required. The map
 * lifecycle is forwarded from the host [LocalLifecycleOwner] so tiles pause/resume with the screen.
 *
 * Zoom is locked (fixed Asia-Pacific overview). Tapping a marker invokes [onRegionClick] with that
 * region's ISO country code so the caller can connect to a volunteer there.
 */
@Composable
fun ExitNodeMap(
    regions: List<ExitNodeRegion>,
    onRegionClick: (countryCode: String) -> Unit,
    modifier: Modifier = Modifier,
) {
    val context = LocalContext.current
    val lifecycleOwner = LocalLifecycleOwner.current
    val currentOnRegionClick by rememberUpdatedState(onRegionClick)

    // MapLibre must be initialised before a MapView is constructed; do both once per composition.
    // Texture mode renders into a TextureView so the map clips to the panel's rounded corners.
    val mapView = remember {
        MapLibre.getInstance(context)
        val options = MapLibreMapOptions.createFromAttributes(context).apply { textureMode(true) }
        MapView(context, options).apply { onCreate(null) }
    }
    var style by remember { mutableStateOf<Style?>(null) }

    DisposableEffect(lifecycleOwner, mapView) {
        val observer = LifecycleEventObserver { _, event ->
            when (event) {
                Lifecycle.Event.ON_START -> mapView.onStart()
                Lifecycle.Event.ON_RESUME -> mapView.onResume()
                Lifecycle.Event.ON_PAUSE -> mapView.onPause()
                Lifecycle.Event.ON_STOP -> mapView.onStop()
                else -> Unit
            }
        }
        lifecycleOwner.lifecycle.addObserver(observer)

        mapView.getMapAsync { map ->
            map.uiSettings.apply {
                isRotateGesturesEnabled = false
                isTiltGesturesEnabled = false
                isCompassEnabled = false
                // Requirement: the map is a fixed overview — not zoomable by any gesture.
                isZoomGesturesEnabled = false
                isDoubleTapGesturesEnabled = false
                isQuickZoomGesturesEnabled = false
            }
            // Pin the zoom level so nothing (fling, programmatic, accessibility) can change scale.
            map.setMinZoomPreference(ASIA_PACIFIC_ZOOM)
            map.setMaxZoomPreference(ASIA_PACIFIC_ZOOM)
            map.moveCamera(CameraUpdateFactory.newLatLngZoom(ASIA_PACIFIC_CENTER, ASIA_PACIFIC_ZOOM))

            // Tap a marker (generous hit box around the dot) -> connect to that country.
            map.addOnMapClickListener { latLng ->
                val point = map.projection.toScreenLocation(latLng)
                val pad = 28f
                val box = RectF(point.x - pad, point.y - pad, point.x + pad, point.y + pad)
                val code = map.queryRenderedFeatures(box, NODE_HALO_LAYER, NODE_CORE_LAYER)
                    .firstNotNullOfOrNull { it.getStringProperty("code") }
                if (code != null) {
                    currentOnRegionClick(code)
                    true
                } else {
                    false
                }
            }

            map.setStyle(Style.Builder().fromJson(cleanMapStyleJson())) { loaded ->
                ensureNodeLayers(loaded)
                style = loaded
            }
        }

        onDispose {
            // Stop forwarding lifecycle events, then tear the map down. onDestroy() handles teardown
            // from any state, so we don't call onStop() here (the observer already did on ON_STOP,
            // and calling it twice is unsafe).
            lifecycleOwner.lifecycle.removeObserver(observer)
            mapView.onDestroy()
            style = null
        }
    }

    // Re-publish markers whenever the regions or the loaded style change.
    LaunchedEffect(regions, style) {
        style?.let { updateNodeSource(it, regions) }
    }

    AndroidView(factory = { mapView }, modifier = modifier)
}

private fun ensureNodeLayers(style: Style) {
    if (style.getSource(NODE_SOURCE) == null) {
        style.addSource(GeoJsonSource(NODE_SOURCE, FeatureCollection.fromFeatures(emptyList())))
    }
    if (style.getLayer(NODE_HALO_LAYER) == null) {
        style.addLayer(
            CircleLayer(NODE_HALO_LAYER, NODE_SOURCE).withProperties(
                PropertyFactory.circleRadius(18f),
                PropertyFactory.circleColor(NODE_GREEN),
                PropertyFactory.circleOpacity(0.18f),
            ),
        )
    }
    if (style.getLayer(NODE_CORE_LAYER) == null) {
        style.addLayer(
            CircleLayer(NODE_CORE_LAYER, NODE_SOURCE).withProperties(
                PropertyFactory.circleRadius(6f),
                PropertyFactory.circleColor(NODE_GREEN),
                PropertyFactory.circleStrokeColor(NODE_STROKE),
                PropertyFactory.circleStrokeWidth(2f),
            ),
        )
    }
    if (style.getLayer(NODE_COUNT_LAYER) == null) {
        // Render the per-country node count above each marker so a dot's meaning is legible
        // (the demotiles basemap has no place labels). "{count}" pulls the Feature property.
        style.addLayer(
            SymbolLayer(NODE_COUNT_LAYER, NODE_SOURCE).withProperties(
                PropertyFactory.textField("{count}"),
                PropertyFactory.textFont(arrayOf("Open Sans Semibold")),
                PropertyFactory.textSize(11f),
                PropertyFactory.textColor(NODE_GREEN),
                PropertyFactory.textHaloColor(NODE_STROKE),
                PropertyFactory.textHaloWidth(1.4f),
                PropertyFactory.textOffset(arrayOf(0f, -1.6f)),
                PropertyFactory.textAllowOverlap(true),
                PropertyFactory.textIgnorePlacement(true),
            ),
        )
    }
}

private fun updateNodeSource(style: Style, regions: List<ExitNodeRegion>) {
    val source = style.getSourceAs<GeoJsonSource>(NODE_SOURCE) ?: return
    val features = regions.map { region ->
        Feature.fromGeometry(Point.fromLngLat(region.longitude, region.latitude)).apply {
            addStringProperty("code", region.countryCode)
            addStringProperty("name", region.countryName)
            addNumberProperty("count", region.nodeCount)
        }
    }
    source.setGeoJson(FeatureCollection.fromFeatures(features))
}
