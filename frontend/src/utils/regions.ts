import L from 'leaflet';
import type { ProvinceGeometry } from './province';

export interface RegionFeature {
  type: 'Feature'; id: string; geometry: ProvinceGeometry;
  properties: { region_id: string; name: string; label: [number, number]; kind: string };
}
export interface RegionCollection {
  type: 'FeatureCollection'; features: RegionFeature[];
  boundary: ProvinceGeometry; bounds: [number, number, number, number];
}
export interface DisplaySource {
  id: string; region_id: string; year: number; label?: string;
  type: 'xyz' | 'tms' | 'wms'; url: string; bounds?: [number, number, number, number];
  min_zoom?: number; max_zoom?: number; attribution?: string;
  layers?: string; format?: string; version?: string; crs?: 'EPSG:3857' | 'EPSG:4326';
}
export interface RegionImagery {
  region_id: string; name: string; years: number[]; selected_year: number | null; sources: DisplaySource[];
}
export interface ImageryScene {
  id: string; region_id: string; region: string; file: string; filename: string;
  date?: string; date_precision: string; date_source?: string; warning?: string;
  bounds?: [number, number, number, number]; resolution_m?: number;
  available: boolean; error?: string; version?: string;
}
export interface RegionScenes { region_id: string; name: string; scenes: ImageryScene[]; phases: ImageryPhase[] }
export interface ImageryPhase {
  id: string; region_id: string; month: string; scenes: ImageryScene[];
  version: string; available: boolean; bounds: [number, number, number, number] | null;
}
export async function getRegionData<T>(url: string, signal?: AbortSignal): Promise<T> {
  const response = await fetch(url, { signal, cache: 'no-store' });
  if (!response.ok) {
    const data = await response.json().catch(() => null);
    throw new Error(typeof data?.detail === 'string' ? data.detail : '行政区数据加载失败');
  }
  return response.json();
}
/** All sources use WGS84 extents and a declared, standard tile grid. */
export function displaySourceLayer(source: DisplaySource): L.TileLayer {
  const options: L.TileLayerOptions = {
    minZoom: source.min_zoom ?? 0, maxZoom: source.max_zoom ?? 20, noWrap: true,
    attribution: source.attribution || '',
    bounds: source.bounds ? [[source.bounds[1], source.bounds[0]], [source.bounds[3], source.bounds[2]]] : undefined,
  };
  if (source.type === 'wms') return L.tileLayer.wms(source.url, {
    ...options, layers: source.layers!, format: source.format || 'image/png', transparent: true,
    version: source.version || '1.1.1', crs: source.crs === 'EPSG:4326' ? L.CRS.EPSG4326 : L.CRS.EPSG3857,
  });
  return L.tileLayer(source.url, { ...options, tms: source.type === 'tms' });
}
