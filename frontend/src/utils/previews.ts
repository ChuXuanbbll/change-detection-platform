import L from 'leaflet';
import { getImageOverlay, getMaskOverlay } from './api';

export type Bounds = [number, number, number, number]; // south, west, north, east
export interface RasterPreview { url: string; bounds: Bounds; crs: 'EPSG:4326' | 'EPSG:3857' }
const cache = new Map<string, Promise<RasterPreview>>();
function cached(key: string, request: () => Promise<RasterPreview>) {
  let pending = cache.get(key);
  if (!pending) {
    pending = request().catch(error => { cache.delete(key); throw error; });
    cache.set(key, pending);
    if (cache.size > 32) cache.delete(cache.keys().next().value!);
  }
  return pending;
}
function preview(url: string, bounds: Bounds, crs?: string): RasterPreview {
  const code = crs || 'EPSG:4326';
  if (code !== 'EPSG:4326' && code !== 'EPSG:3857') throw new Error(`当前预览不支持 ${code} 坐标系`);
  if (code === 'EPSG:3857') {
    const sw = L.CRS.EPSG3857.unproject(L.point(bounds[1], bounds[0]));
    const ne = L.CRS.EPSG3857.unproject(L.point(bounds[3], bounds[2]));
    bounds = [sw.lat, sw.lng, ne.lat, ne.lng];
  }
  if (!bounds.every(Number.isFinite) || bounds[0] >= bounds[2] || bounds[1] >= bounds[3] || bounds[0] < -90 || bounds[2] > 90) throw new Error('影像地理范围无效');
  return { url, bounds, crs: code };
}
export function imagePreview(path: string) {
  return cached(`image:${path}`, async () => {
    const data = await getImageOverlay(path, 2048);
    return preview(`data:image/png;base64,${data.image_b64}`, data.bounds, data.crs);
  });
}
export function maskPreview(path: string, color: string) {
  return cached(`mask:${path}:${color}`, async () => {
    const data = await getMaskOverlay(path, 1, color);
    return preview(`data:image/png;base64,${data.png_b64}`, data.bounds, data.crs);
  });
}
export function leafletBounds(b: Bounds): L.LatLngBoundsLiteral { return [[b[0], b[1]], [b[2], b[3]]]; }
export function previewProjection(crs?: string) { return crs === 'EPSG:3857' ? L.CRS.EPSG3857 : L.CRS.EPSG4326; }
