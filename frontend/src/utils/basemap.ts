import L from 'leaflet';
import { provincePolygons, type ProvinceGeometry } from './province';

export const HEBEI_BOUNDS: L.LatLngBoundsLiteral = [[36.0, 113.45], [42.65, 119.9]];
export const LOCAL_MAP_OPTIONS: L.MapOptions = {
  attributionControl: false,
  center: [39.2, 116.5], zoom: 6, minZoom: 6, maxZoom: 20,
  zoomSnap: 0.25, zoomDelta: 0.5,
  maxBounds: HEBEI_BOUNDS, maxBoundsViscosity: 1,
};
export interface LocalScene {
  id: string; region: string; date: string; date_precision: string;
  bounds: [number,number,number,number]; width: number; height: number;
}
export interface LocalCatalog { images: LocalScene[]; warning: string | null; bounds: [number,number,number,number]; boundary: ProvinceGeometry }
export async function getLocalCatalog(): Promise<LocalCatalog> {
  const response = await fetch('/py/local/catalog');
  if (!response.ok) {
    const error = await response.json().catch(() => null);
    throw new Error(typeof error?.detail === 'string' ? error.detail : '本地影像目录加载失败，请检查 Python 服务和项目数据');
  }
  return response.json();
}
export function addCoverage(map: L.Map, data: Pick<LocalCatalog, 'boundary' | 'bounds'>): void {
  const boundary = data.boundary;
  const [west, south, east, north] = data.bounds;
  map.setMaxBounds([[south, west], [north, east]]);
  const pane = map.createPane('province-mask');
  pane.style.zIndex = '650';
  pane.style.pointerEvents = 'none';
  // Even-odd fill: world minus province, retaining Beijing/Tianjin and polygon holes.
  const rings: L.LatLngExpression[][] = [[[-85,-180],[-85,180],[85,180],[85,-180]]];
  for (const polygon of provincePolygons(boundary)) {
    for (const ring of polygon) rings.push(ring.map(([lng, lat]) => [lat,lng] as L.LatLngTuple));
  }
  L.polygon(rings, { pane: 'province-mask', stroke: false, fillColor: '#152330',
    fillOpacity: 1, fillRule: 'evenodd', interactive: false }).addTo(map);
  L.geoJSON(boundary, { pane: 'province-mask', interactive: false,
    style: { color: '#94a3b8', weight: 1, fillOpacity: 0 } }).addTo(map);
}
