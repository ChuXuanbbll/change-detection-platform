import clipping from 'polygon-clipping';
import type { ProvinceGeometry } from './province';
import type { AreaBounds, AreaPolygon } from './api';

export type Position = [number, number];
export const BOUNDARY_EPSILON = 1e-9;
const cross = (a: Position, b: Position) => a[0] * b[1] - a[1] * b[0];
const subtract = (a: Position, b: Position): Position => [a[0] - b[0], a[1] - b[1]];
const interpolate = (a: Position, b: Position, t: number): Position => [a[0] + t * (b[0] - a[0]), a[1] + t * (b[1] - a[1])];
const rings = (region: ProvinceGeometry) => (region.type === 'Polygon' ? [region.coordinates] : region.coordinates);

export function samePoint(a: Position, b: Position): boolean {
  return Math.hypot(a[0] - b[0], a[1] - b[1]) <= BOUNDARY_EPSILON;
}

function onSegment(p: Position, a: Position, b: Position): boolean {
  const d = subtract(b, a), length = Math.hypot(...d);
  if (!length) return samePoint(p, a);
  const t = ((p[0] - a[0]) * d[0] + (p[1] - a[1]) * d[1]) / (length * length);
  return t >= 0 && t <= 1 && Math.abs(cross(d, subtract(p, a))) / length <= BOUNDARY_EPSILON;
}

function pointInRing(p: Position, ring: number[][]): boolean {
  let inside = false;
  for (let i = 0, j = ring.length - 1; i < ring.length; j = i++) {
    const a = ring[i], b = ring[j];
    if ((a[1] > p[1]) !== (b[1] > p[1]) && p[0] < (b[0] - a[0]) * (p[1] - a[1]) / (b[1] - a[1]) + a[0]) inside = !inside;
  }
  return inside;
}

/** Boundary points are allowed, including hole boundaries. */
export function regionCoversPoint(region: ProvinceGeometry, p: Position): boolean {
  return rings(region).some(polygon => polygon.some(ring => ring.some((point, i) =>
    onSegment(p, point as Position, ring[(i + 1) % ring.length] as Position))) ||
    (pointInRing(p, polygon[0]) && !polygon.slice(1).some(ring => pointInRing(p, ring))));
}

/** Return all segment intersection parameters, including collinear overlap ends. */
function intersections(a: Position, b: Position, c: Position, d: Position): number[] {
  const r = subtract(b, a), s = subtract(d, c), q = subtract(c, a);
  const denominator = cross(r, s);
  const scale = Math.hypot(...r) * Math.hypot(...s);
  if (Math.abs(denominator) > Number.EPSILON * Math.max(scale, Number.MIN_VALUE) * 16) {
    const t = cross(q, s) / denominator, u = cross(q, r) / denominator;
    return t >= -1e-12 && t <= 1 + 1e-12 && u >= -1e-12 && u <= 1 + 1e-12 ? [Math.max(0, Math.min(1, t))] : [];
  }
  const lengthSquared = r[0] ** 2 + r[1] ** 2;
  if (!lengthSquared || Math.abs(cross(q, r)) / Math.sqrt(lengthSquared) > BOUNDARY_EPSILON) return [];
  const ends = [c, d].map(p => ((p[0] - a[0]) * r[0] + (p[1] - a[1]) * r[1]) / lengthSquared);
  const start = Math.max(0, Math.min(...ends)), end = Math.min(1, Math.max(...ends));
  return start <= end ? [start, end] : [];
}

/** Split at every boundary crossing; stop at the first outside interval. */
export function constrainToRegion(region: ProvinceGeometry, previous: Position, cursor: Position): Position {
  if (samePoint(previous, cursor)) return [...previous];
  const cuts = [0, 1];
  for (const polygon of rings(region)) for (const ring of polygon) {
    for (let i = 0; i < ring.length; i++) cuts.push(...intersections(previous, cursor, ring[i] as Position, ring[(i + 1) % ring.length] as Position));
  }
  const sorted = [...new Set(cuts)].sort((a, b) => a - b);
  for (let i = 0; i < sorted.length - 1; i++) {
    if (!regionCoversPoint(region, interpolate(previous, cursor, (sorted[i] + sorted[i + 1]) / 2))) {
      return interpolate(previous, cursor, sorted[i]);
    }
  }
  return [...cursor];
}

export function polygonFromPoints(points: Position[]): AreaPolygon {
  return { type: 'Polygon', coordinates: [[...points.map(p => [...p] as Position), [...points[0]] as Position]] };
}

export function polygonBounds(polygon: AreaPolygon): AreaBounds {
  const ring = polygon.coordinates[0];
  return { west: Math.min(...ring.map(p => p[0])), east: Math.max(...ring.map(p => p[0])),
    south: Math.min(...ring.map(p => p[1])), north: Math.max(...ring.map(p => p[1])) };
}

export function rectanglePoints(bounds: AreaBounds): Position[] {
  return [[bounds.west, bounds.south], [bounds.east, bounds.south],
    [bounds.east, bounds.north], [bounds.west, bounds.north]];
}

/** Existing box crops include only the administrative intersection. Preserve that
 * footprint when converting an older box to the single-ring polygon editor.
 */
export function rectangleEditPoints(bounds: AreaBounds, region: ProvinceGeometry): Position[] | null {
  const polygons = clipping.intersection(polygonFromPoints(rectanglePoints(bounds)).coordinates as clipping.Polygon,
    rings(region) as clipping.MultiPolygon);
  if (polygons.length !== 1 || polygons[0].length !== 1) return null;
  const points = polygons[0][0].slice(0, -1) as Position[];
  return points.length >= 3 && !polygonError(points, region) ? points : null;
}

/** Translate an edge's supporting line and intersect it with its neighbours.
 * Coordinates can be projected map pixels, so parallelism matches the display.
 * Always calculate from the drag-start ring to avoid cumulative drift.
 */
export function moveParallelEdge(points: Position[], index: number, offset: Position): Position[] | null {
  const n = points.length;
  if (n < 3 || index < 0 || index >= n) return null;
  const a = points[index], b = points[(index + 1) % n];
  const previous = points[(index + n - 1) % n], following = points[(index + 2) % n];
  const direction = subtract(b, a);
  const translated: Position = [a[0] + offset[0], a[1] + offset[1]];
  const intersectLine = (origin: Position, adjacent: Position): Position | null => {
    const vector = subtract(adjacent, origin), denominator = cross(vector, direction);
    if (Math.abs(denominator) <= 1e-12 * Math.hypot(...vector) * Math.hypot(...direction)) return null;
    const result = interpolate(origin, adjacent, cross(subtract(translated, origin), direction) / denominator);
    return result.every(Number.isFinite) ? result : null;
  };
  const left = intersectLine(previous, a), right = intersectLine(following, b);
  if (!left || !right) return null;
  const dot = (u: Position, v: Position) => u[0] * v[0] + u[1] * v[1];
  // Do not let an endpoint pass its fixed neighbour or invert/collapse the edge.
  if (dot(subtract(right, left), direction) <= 0 ||
      dot(subtract(left, previous), subtract(a, previous)) <= 0 ||
      dot(subtract(right, following), subtract(b, following)) <= 0) return null;
  return points.map((p, i) => i === index ? left : i === (index + 1) % n ? right : [...p]);
}

export function polygonError(points: Position[], region: ProvinceGeometry): string {
  if (points.length < 3) return '至少需要三个不同顶点';
  if (points.some((p, i) => points.some((q, j) => i !== j && samePoint(p, q)))) return '顶点不能重复';
  for (let i = 0; i < points.length; i++) {
    const a = points[i], b = points[(i + 1) % points.length], c = points[(i + 2) % points.length];
    if (onSegment(c, a, b) || onSegment(a, b, c)) return '相邻边不能折返重叠';
    for (let j = i + 2; j < points.length; j++) {
      if (i === 0 && j === points.length - 1) continue;
      if (intersections(a, b, points[j], points[(j + 1) % points.length]).length) return '多边形不能自交';
    }
    if (!regionCoversPoint(region, a) || !samePoint(constrainToRegion(region, a, b), b)) return '边界越出当前行政区，请补点或调整顶点';
  }
  // Translate to the first point to avoid cancellation at large longitudes.
  const origin = points[0];
  const twiceArea = points.reduce((sum, p, i) => sum + cross(subtract(p, origin), subtract(points[(i + 1) % points.length], origin)), 0);
  if (Math.abs(twiceArea) <= BOUNDARY_EPSILON ** 2) return '多边形面积必须大于零';
  const difference = clipping.difference(polygonFromPoints(points).coordinates as clipping.Polygon,
    rings(region) as clipping.MultiPolygon);
  // Ignore numerical slivers narrower than the documented boundary tolerance.
  for (const polygon of difference) for (const ring of polygon) {
    const area = Math.abs(ring.reduce((sum, p, i) => sum + cross(subtract(p, ring[0]), subtract(ring[(i + 1) % ring.length], ring[0])), 0)) / 2;
    const perimeter = ring.reduce((sum, p, i) => sum + Math.hypot(...subtract(p, ring[(i + 1) % ring.length])), 0);
    if (area > BOUNDARY_EPSILON * perimeter) return '多边形内部包含行政区外区域或孔洞，请调整顶点';
  }
  return '';
}
