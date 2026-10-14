import axios, { AxiosError, AxiosResponse } from 'axios';

export interface AreaBounds { west: number; south: number; east: number; north: number }
export type AreaPolygon = GeoJSON.Polygon;
export interface ConfidenceHistograms {
  bin_edges: number[];
  changed_counts: number[];
  all_valid_counts: number[];
  changed_sample_count: number;
  all_valid_sample_count: number;
}
export interface DetectionStats {
  confidence_histograms?: ConfidenceHistograms;
  changed_pixels?: number;
  total_pixels?: number;
  change_ratio?: number;
  mean_confidence?: number;
  feature_count?: number;
  changed_area_pixels?: number;
  changed_area_m2?: number;
  mean_confidence_all?: number;
}

export interface ChangePatchProperties {
  patch_id?: string;
  value?: number;
  change_type?: string;
  area_pixels?: number;
  area_m2?: number;
  area_ha?: number;
  perimeter_m?: number;
  perimeter_crs?: string;
  centroid?: [number, number];
  confidence_mean?: number | null;
  confidence_valid_pixels?: number;
}

export type ChangePatch = GeoJSON.Feature<GeoJSON.Polygon | GeoJSON.MultiPolygon, ChangePatchProperties>;
export interface ChangePatchCollection extends GeoJSON.FeatureCollection<GeoJSON.Polygon | GeoJSON.MultiPolygon, ChangePatchProperties> {
  schema_version?: number;
  source_crs?: string;
  area_method?: string;
  confidence_definition?: string;
}

export interface CreateTaskPayload {
  batch_id?: string;
  algorithm: string;
  params: Record<string, number | string | boolean>;
  before_id?: string;
  after_id?: string;
  aoi?: { type: string; coordinates: number[][][] };
}

export interface TaskInfo {
  id: string;
  status: 'pending' | 'running' | 'completed' | 'succeeded' | 'failed';
  algorithm: string;
  progress?: number;
  stage?: string;
  result?: Record<string, unknown>;
  error_msg?: string;
  started_at?: string;
  finished_at?: string;
  created_at?: string;
}

export interface TaskResult {
  task_id: string;
  mask_path?: string;
  confidence_path?: string;
  geojson_path?: string;
  stats_json?: string;
  geojson?: ChangePatchCollection;
  geojson_error?: string;
  created_at?: string;
}

export interface ImageInfo {
  id: string;
  source: 'local' | 'wayback' | 'upload';
  phase: string;
  capture_date?: string;
  file_path: string;
  thumb_path: string;
  crs?: string;
  bounds_west?: number;
  bounds_south?: number;
  bounds_east?: number;
  bounds_north?: number;
  width?: number;
  height?: number;
  bands?: number;
  dtype?: string;
  meta_json?: string;
  created_at: string;
}

export interface LocalFetchPayload {
  aoi?: AreaPolygon;
  progress_id?: string;
  model_id?: string;
  config_revision?: number;
  params?: Record<string, number | string | boolean>;
  bounds: [number, number, number, number];  // w, s, e, n
  start_date?: string;
  end_date?: string;
  max_images?: number;
  zoom?: number;
  buffer?: number;
  region_id?: string;
  scene_ids?: string[];
}

const http = axios.create({
  baseURL: '/api',
  // GeoTIFF 可能数百 MB，上传和服务端元数据读取不能限制在 60 秒内。
  timeout: 10 * 60 * 1000
});

/**
 * 把后端 `{error: ..., code: ..., ...}` 提升为 Error.message，让 catch 的 e.message
 * 直接是人类可读的错误文本，避免 "Request failed with status code 400"。
 */
function extractErrorMessage(err: AxiosError): string {
  const data = err.response?.data as Record<string, unknown> | undefined;
  if (data && typeof data === 'object') {
    const e = (data as { error?: unknown }).error;
    if (typeof e === 'string' && e.length > 0) return e;
    const detail = (data as { detail?: unknown }).detail;
    if (typeof detail === 'string' && detail.length > 0) return detail;
    return JSON.stringify(data);
  }
  return err.message;
}

http.interceptors.response.use(
  (resp: AxiosResponse) => resp,
  async (err: AxiosError) => {
    // 文件下载错误仍是 JSON，但 responseType: 'blob' 会将它包装成 Blob。
    if (err.response?.data instanceof Blob) {
      try { err.response.data = JSON.parse(await err.response.data.text()); } catch { /* 保留原始错误 */ }
    }
    const msg = extractErrorMessage(err);
    console.error('[API Error]', err.config?.url, msg);
    // 保留状态码和 RESULT_NOT_READY，供结果读取重试判断。
    err.message = msg;
    return Promise.reject(err);
  }
);

export function createTask(payload: CreateTaskPayload): Promise<TaskInfo> {
  return http.post<TaskInfo>('/tasks', payload).then((r) => r.data);
}

export function getTask(taskId: string): Promise<TaskInfo> {
  return http.get<TaskInfo>(`/tasks/${taskId}`, { timeout: 10000 }).then((r) => r.data);
}

export type ResultFormat = 'geojson' | 'geotiff' | 'csv' | 'shp';

// 前端 format → 后端 result/:file 文件名映射。
const RESULT_FILE: Record<ResultFormat, string> = {
  geojson: 'change.geojson',
  shp: 'change.zip',
  geotiff: 'mask.tif',
  csv: 'stats.csv'
};

export function getTaskResult(taskId: string): Promise<TaskResult> {
  return http.get<TaskResult>(`/tasks/${taskId}/result`).then((r) => r.data);
}

export function downloadTaskResult(
  taskId: string,
  format: ResultFormat
): Promise<Blob> {
  return http
    .get<Blob>(`/tasks/${taskId}/result/${RESULT_FILE[format]}`, { responseType: 'blob' })
    .then((r) => r.data);
}

export interface PreparationProgress {
  id: string;
  status: 'running' | 'succeeded' | 'failed';
  stage: string;
  progress: number;
}
export function getLocalProgress(id: string): Promise<PreparationProgress> {
  return http.get(`/images/local/progress/${encodeURIComponent(id)}`, { timeout: 10000 }).then(response => response.data);
}

export function fetchLocalImagery(payload: LocalFetchPayload): Promise<{ items: ImageInfo[]; count: number; batch_id: string }> {
  return http.post('/images/local', payload, { timeout: 0 }).then((r) => r.data);
}

export interface MaskOverlayResult {
  png_b64: string;
  /** 地理 bounds，顺序 [south, west, north, east]（已由后端 [w,s,e,n] 规范）。 */
  bounds: [number, number, number, number];
  width: number;
  height: number;
  crs?: string;
  alpha?: number;
  color?: string;
}

/**
 * 请求 Python 服务（:8090，dev 走 vite proxy `/py`）把 mask 栅格渲染为
 * 透明 RGBA PNG 叠加图，返回 base64 PNG + 地理 bounds。
 */
export async function getMaskOverlay(
  maskPath: string,
  alpha = 0.45,
  color = '#ff0000'
): Promise<MaskOverlayResult> {
  const resp = await fetch('/py/overlay/mask', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ mask_path: maskPath, alpha, color })
  });
  if (!resp.ok) {
    const text = await resp.text().catch(() => '');
    throw new Error(`获取 mask 叠加失败 (${resp.status}): ${text}`);
  }
  const data: MaskOverlayResult = await resp.json();
  // 后端返回 [west, south, east, north]，规范化为 [south, west, north, east]，
  // 便于直接构造 L.latLngBounds([s,w],[n,e])。
  const b = data.bounds as number[];
  const bounds: [number, number, number, number] = [b[1], b[0], b[3], b[2]];
  return {
    ...data,
    bounds
  };
}

export interface ImageOverlayResult {
  image_b64: string;
  /** 地理 bounds，顺序 [west, south, east, north]。 */
  bounds: [number, number, number, number];
  width: number;
  height: number;
  crs?: string;
}

/**
 * 请求 Python 服务把 RGB GeoTIFF 渲染为可叠加显示的 PNG，返回 base64 + bounds。
 * 用于中央地图显示当前批次的独立时相底图图层。
 */
export async function getImageOverlay(imagePath: string, maxEdge = 512): Promise<ImageOverlayResult> {
  const resp = await fetch('/py/overlay/image', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ image_path: imagePath, max_edge: maxEdge })
  });
  if (!resp.ok) {
    const text = await resp.text().catch(() => '');
    throw new Error(`获取影像叠加失败 (${resp.status}): ${text}`);
  }
  const data: ImageOverlayResult = await resp.json();
  const b = data.bounds as number[];
  // 后端返回 [west, south, east, north]，规范化为 [south, west, north, east]。
  data.bounds = [b[1], b[0], b[3], b[2]];
  return data;
}

export default http;
