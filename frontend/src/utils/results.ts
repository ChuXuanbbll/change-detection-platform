import type { ChangePatch, ConfidenceHistograms, DetectionStats } from './api';
import type { TimelinePair } from './timeline';

export const statusLabel = (status: string) => ({ waiting: '等待中', pending: '排队中', running: '检测中', succeeded: '已完成', failed: '失败' }[status] || status);
export function formatArea(value?: number): string {
  if (value == null || !Number.isFinite(value)) return '—';
  return value < 1e6 ? `${value.toLocaleString('zh-CN', { maximumFractionDigits: 1 })} m²` : `${(value / 1e6).toLocaleString('zh-CN', { maximumFractionDigits: 3 })} km²`;
}
export function formatScore(value?: number | null): string {
  return value == null || !Number.isFinite(value) ? '—' : value.toFixed(3);
}
export function meanChangeScore(stats?: DetectionStats): string {
  if (stats?.changed_pixels === 0 || stats?.confidence_histograms?.changed_sample_count === 0) return '—';
  return formatScore(stats?.mean_confidence);
}
export function rankedPatches(pair?: TimelinePair): ChangePatch[] {
  return [...(pair?.geojson?.features || [])].sort((a, b) =>
    (b.properties.area_m2 ?? -1) - (a.properties.area_m2 ?? -1) || patchId(a).localeCompare(patchId(b), 'en', { numeric: true }));
}
export function patchId(patch: ChangePatch): string { return String(patch.properties.patch_id ?? patch.id ?? ''); }
export function patchBounds(patch: ChangePatch): [number, number, number, number] | undefined {
  const rings = patch.geometry.type === 'Polygon' ? patch.geometry.coordinates : patch.geometry.coordinates.flat();
  const points = rings.flat().filter(p => Number.isFinite(p[0]) && Number.isFinite(p[1]));
  if (!points.length) return;
  let west = Infinity, south = Infinity, east = -Infinity, north = -Infinity;
  for (const [x, y] of points) { west = Math.min(west, x); east = Math.max(east, x); south = Math.min(south, y); north = Math.max(north, y); }
  return [south, west, north, east];
}
export function parseDetectionStats(raw: Record<string, unknown>): DetectionStats {
  // Some persisted/API responses wrap the Python stats payload in `stats`.
  // Normalize that shape before validating optional histogram data so a valid
  // distribution is not silently discarded as unavailable.
  const payload = raw.stats && typeof raw.stats === 'object' && !Array.isArray(raw.stats)
    ? raw.stats as Record<string, unknown>
    : raw;
  const stats: DetectionStats = Object.fromEntries(['changed_pixels', 'total_pixels', 'change_ratio', 'mean_confidence', 'mean_confidence_all', 'feature_count', 'changed_area_pixels', 'changed_area_m2']
    .filter(key => typeof payload[key] === 'number' && Number.isFinite(payload[key])).map(key => [key, payload[key]]));
  const candidate = payload.confidence_histograms as Partial<ConfidenceHistograms> | undefined;
  const integerCounts = (value: unknown) => Array.isArray(value) && value.length === 20 && value.every(n => typeof n === 'number' && Number.isSafeInteger(n) && n >= 0);
  if (candidate && Array.isArray(candidate.bin_edges) && candidate.bin_edges.length === 21 &&
    candidate.bin_edges.every((n, i) => typeof n === 'number' && Number.isFinite(n) && Math.abs(n - i / 20) < 1e-6) &&
    integerCounts(candidate.changed_counts) && integerCounts(candidate.all_valid_counts)) {
    const changed = candidate.changed_counts as number[];
    const all = candidate.all_valid_counts as number[];
    const changedCount = changed.reduce((a, b) => a + b, 0);
    const allCount = all.reduce((a, b) => a + b, 0);
    if ((candidate.changed_sample_count != null && candidate.changed_sample_count !== changedCount) ||
      (candidate.all_valid_sample_count != null && candidate.all_valid_sample_count !== allCount)) return stats;
    // Derive sample counts from bins so older producers that omitted them still render.
    stats.confidence_histograms = { bin_edges: candidate.bin_edges, changed_counts: changed, all_valid_counts: all,
      changed_sample_count: changedCount, all_valid_sample_count: allCount };
  }
  return stats;
}
