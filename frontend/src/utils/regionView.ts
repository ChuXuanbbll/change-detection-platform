/** Pixel-space navigation constraints, independent of Leaflet and the DOM. */
export interface ViewPoint { x: number; y: number }
export type ViewPolygon = ViewPoint[][];
export interface ViewExtent { min: ViewPoint; max: ViewPoint }
const clamp = (value: number, low: number, high: number) => Math.max(low, Math.min(high, value));

export function centerLimits(extent: ViewExtent, size: ViewPoint, padding = 32): ViewExtent {
  const axis = (a: number, b: number, length: number): [number, number] => {
    const half = length / 2 - Math.min(padding, length / 4);
    // Small regions remain entirely visible; large regions fill this viewport axis.
    return a + half >= b - half ? [b - half, a + half] : [a + half, b - half];
  };
  const [left, right] = axis(extent.min.x, extent.max.x, size.x);
  const [top, bottom] = axis(extent.min.y, extent.max.y, size.y);
  return { min: { x: left, y: top }, max: { x: right, y: bottom } };
}

function inRing(point: ViewPoint, ring: ViewPoint[]): boolean {
  let inside = false;
  for (let i = 0, j = ring.length - 1; i < ring.length; j = i++) {
    const a = ring[i], b = ring[j];
    if ((a.y > point.y) !== (b.y > point.y) &&
      point.x < (b.x - a.x) * (point.y - a.y) / (b.y - a.y) + a.x) inside = !inside;
  }
  return inside;
}

function segmentInRect(a: ViewPoint, b: ViewPoint, rect: ViewExtent): boolean {
  let start = 0, end = 1;
  for (const axis of ['x', 'y'] as const) {
    const delta = b[axis] - a[axis];
    if (!delta) {
      if (a[axis] < rect.min[axis] || a[axis] > rect.max[axis]) return false;
    } else {
      const first = (rect.min[axis] - a[axis]) / delta, last = (rect.max[axis] - a[axis]) / delta;
      start = Math.max(start, Math.min(first, last)); end = Math.min(end, Math.max(first, last));
      if (start > end) return false;
    }
  }
  return true;
}

/** Includes crossing edges, holes and detached parts, not just bbox overlap. */
export function regionVisible(polygons: ViewPolygon[], center: ViewPoint, size: ViewPoint, padding = 32): boolean {
  const half = { x: size.x / 2 - Math.min(padding, size.x / 4), y: size.y / 2 - Math.min(padding, size.y / 4) };
  const rect = { min: { x: center.x - half.x, y: center.y - half.y }, max: { x: center.x + half.x, y: center.y + half.y } };
  const corners = [rect.min, rect.max, { x: rect.min.x, y: rect.max.y }, { x: rect.max.x, y: rect.min.y }];
  return polygons.some(rings => {
    if (!rings[0]?.length) return false;
    if (corners.some(point => inRing(point, rings[0]) && !rings.slice(1).some(ring => inRing(point, ring)))) return true;
    return rings.some(ring => ring.some((point, i) => segmentInRect(point, ring[(i + 1) % ring.length], rect)));
  });
}

export function constrainRegionCenter(center: ViewPoint, size: ViewPoint, extent: ViewExtent, polygons: ViewPolygon[]): ViewPoint {
  const limits = centerLimits(extent, size);
  const clampPoint = (point: ViewPoint): ViewPoint => ({
    x: clamp(point.x, limits.min.x, limits.max.x), y: clamp(point.y, limits.min.y, limits.max.y),
  });
  const target = clampPoint(center);
  if (regionVisible(polygons, target, size)) return target;
  // A bbox corner or polygon hole may contain no city. Find the closest boundary
  // point and place it inside the viewport, preserving the per-axis limits.
  let nearest = target, distance = Infinity;
  for (const rings of polygons) for (const ring of rings) for (let i = 0; i < ring.length; i++) {
    const a = ring[i], b = ring[(i + 1) % ring.length];
    const dx = b.x - a.x, dy = b.y - a.y;
    const t = clamp(((target.x - a.x) * dx + (target.y - a.y) * dy) / (dx * dx + dy * dy || 1), 0, 1);
    const candidate = clampPoint({ x: a.x + dx * t, y: a.y + dy * t });
    const d = (candidate.x - target.x) ** 2 + (candidate.y - target.y) ** 2;
    if (d < distance) { nearest = candidate; distance = d; }
  }
  // Stop at the visibility boundary instead of snapping all the way to the city.
  let valid = nearest, invalid = target;
  for (let i = 0; i < 24; i++) {
    const middle = { x: (valid.x + invalid.x) / 2, y: (valid.y + invalid.y) / 2 };
    if (regionVisible(polygons, middle, size)) valid = middle;
    else invalid = middle;
  }
  return valid;
}
