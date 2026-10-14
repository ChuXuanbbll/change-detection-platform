/** Pure WGS84 polygon containment, including holes and detached province parts. */
export type ProvinceGeometry =
  | { type: 'Polygon'; coordinates: number[][][] }
  | { type: 'MultiPolygon'; coordinates: number[][][][] };

function inRing(ring: number[][], lng: number, lat: number): boolean {
  let inside = false;
  for (let i = 0, j = ring.length - 1; i < ring.length; j = i++) {
    const [xi, yi] = ring[i];
    const [xj, yj] = ring[j];
    if ((yi > lat) !== (yj > lat) && lng < (xj - xi) * (lat - yi) / (yj - yi) + xi) inside = !inside;
  }
  return inside;
}
export function provincePolygons(boundary: ProvinceGeometry): number[][][][] {
  return boundary.type === 'Polygon' ? [boundary.coordinates] : boundary.coordinates;
}
export function containsProvincePoint(boundary: ProvinceGeometry, lat: number, lng: number): boolean {
  return provincePolygons(boundary).some(rings =>
    inRing(rings[0], lng, lat) && !rings.slice(1).some(ring => inRing(ring, lng, lat)));
}
