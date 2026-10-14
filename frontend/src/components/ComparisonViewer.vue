<script setup lang="ts">
import { ref, watch, onMounted, onUnmounted, onActivated, nextTick } from 'vue';
import L from 'leaflet';
import 'leaflet/dist/leaflet.css';
import type { ChangePatch } from '../utils/api';
import { leafletBounds, previewProjection, type Bounds, type RasterPreview } from '../utils/previews';
export interface ViewPosition { lat: number; lng: number; zoom: number }
const props = defineProps<{ title: string; date?: string; image?: RasterPreview; mask?: RasterPreview;
  bounds?: Bounds; focus?: Bounds; selected?: ChangePatch; view?: ViewPosition; loading?: boolean; error?: string }>();
const emit = defineEmits<{ (e: 'view', view: ViewPosition): void }>();
const element = ref<HTMLDivElement | null>(null);
let map: L.Map | undefined;
let layers: L.LayerGroup | undefined;
let selection: L.GeoJSON | undefined;
let observer: ResizeObserver | undefined;
let syncing = false;
let activeCRS = '';
function createMap() {
  if (!element.value?.isConnected) return;
  const crs = props.image?.crs || 'EPSG:4326';
  if (map && activeCRS === crs) return;
  map?.remove();
  activeCRS = crs;
  map = L.map(element.value, { crs: previewProjection(crs), zoomControl: true, attributionControl: false, trackResize: false,
    zoomAnimation: false, fadeAnimation: false, markerZoomAnimation: false, maxZoom: 24, minZoom: 1, zoomSnap: 0.25 });
  layers = L.layerGroup().addTo(map);
  L.control.scale({ imperial: false, position: 'bottomleft' }).addTo(map);
  map.on('moveend', () => {
    if (!map || syncing || !element.value?.isConnected || !element.value.clientWidth) return;
    const center = map.getCenter();
    emit('view', { lat: center.lat, lng: center.lng, zoom: map.getZoom() });
  });
}
function fit(bounds?: Bounds) {
  if (!map || !bounds || !element.value?.clientWidth) return;
  syncing = true;
  map.fitBounds(leafletBounds(bounds), { padding: [18, 18], animate: false, maxZoom: 23 });
  syncing = false;
}
function highlight() {
  selection?.remove();
  if (map && props.selected) selection = L.geoJSON(props.selected, { interactive: false, style: { color: '#fff1a8', weight: 3, opacity: 1, fill: false } }).addTo(map);
}
function resize() {
  if (!map || !element.value?.isConnected || !element.value.clientWidth) return;
  syncing = true;
  map.invalidateSize({ pan: false });
  if (props.view) map.setView([props.view.lat, props.view.lng], props.view.zoom, { animate: false });
  else fit(props.focus || props.bounds);
  syncing = false;
}
function render() {
  if (!element.value?.isConnected) return;
  createMap();
  if (!map || !layers) return;
  layers.clearLayers();
  if (props.image) L.imageOverlay(props.image.url, leafletBounds(props.image.bounds), { opacity: 1 }).addTo(layers);
  if (props.mask && props.image?.crs === props.mask.crs) L.imageOverlay(props.mask.url, leafletBounds(props.mask.bounds), { opacity: 1, className: 'opaque-change-mask' }).addTo(layers);
  if (props.bounds) {
    map.setMaxBounds(L.latLngBounds(leafletBounds(props.bounds)).pad(0.25));
    fit(props.focus || props.bounds);
  }
  highlight();
}
watch(() => [props.image, props.mask, props.bounds], () => { void nextTick(render); });
watch(() => props.focus, bounds => fit(bounds || props.bounds));
watch(() => props.selected, highlight);
watch(() => props.view, view => {
  if (!map || !view) return;
  syncing = true;
  map.setView([view.lat, view.lng], view.zoom, { animate: false });
  syncing = false;
});
onMounted(() => {
  render();
  observer = new ResizeObserver(resize);
  if (element.value) observer.observe(element.value);
});
onActivated(() => { void nextTick(() => { render(); resize(); }); });
onUnmounted(() => {
  observer?.disconnect(); observer = undefined;
  map?.remove(); map = undefined; layers = undefined; selection = undefined;
});
</script>
<template>
  <section class="panel comparison-viewer">
    <header><h2>{{ title }}</h2><span>{{ date || '—' }}</span></header>
    <div class="viewer-frame">
      <div ref="element" class="comparison-map" :aria-label="`${title}地图，可平移和缩放`" />
      <span class="north-marker" aria-hidden="true">↑<small>N</small></span>
      <div v-if="loading || error || !image" class="viewer-message" role="status">{{ loading ? '正在加载影像…' : error || '暂无影像' }}</div>
    </div>
  </section>
</template>
