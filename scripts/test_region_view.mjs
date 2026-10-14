import assert from 'node:assert/strict';
import { centerLimits, constrainRegionCenter, regionVisible } from '../frontend/src/utils/regionView.ts';
const ring = points => points.map(([x, y]) => ({ x, y }));
const extent = { min: { x: 0, y: 0 }, max: { x: 1000, y: 1000 } };
const square = [[ring([[0, 0], [1000, 0], [1000, 1000], [0, 1000], [0, 0]])]];
const large = { x: 1200, y: 1400 }, small = { x: 200, y: 200 };
assert.deepEqual(centerLimits(extent, large), { min: { x: 432, y: 332 }, max: { x: 568, y: 668 } });
for (const x of [-10000, 500, 10000]) for (const y of [-10000, 500, 10000]) {
  const result = constrainRegionCenter({ x, y }, large, extent, square);
  assert.ok(result.x >= 432 && result.x <= 568 && result.y >= 332 && result.y <= 668);
  assert.ok(regionVisible(square, result, large));
}
const hole = [[square[0][0], ring([[200, 200], [800, 200], [800, 800], [200, 800], [200, 200]])]];
const detached = [[ring([[0, 0], [100, 0], [100, 100], [0, 100], [0, 0]])],
  [ring([[900, 900], [1000, 900], [1000, 1000], [900, 1000], [900, 900]])]];
const concave = [[ring([[0, 0], [1000, 0], [1000, 100], [100, 100], [100, 1000], [0, 1000], [0, 0]])]];
for (const polygons of [hole, detached, concave]) {
  assert.equal(regionVisible(polygons, { x: 500, y: 500 }, small), false);
  for (const x of [-1000, 50, 500, 950, 2000]) for (const y of [-1000, 50, 500, 950, 2000]) {
    const result = constrainRegionCenter({ x, y }, small, extent, polygons);
    assert.ok(regionVisible(polygons, result, small), `invalid correction ${JSON.stringify(result)}`);
    assert.ok(result.x >= 68 && result.x <= 932 && result.y >= 68 && result.y <= 932);
  }
}
// Crossing edges are visible even when no vertex or viewport corner is inside.
const strip = [[ring([[-1000, -5], [1000, -5], [1000, 5], [-1000, 5], [-1000, -5]])]];
assert.ok(regionVisible(strip, { x: 0, y: 0 }, small));
console.log('Region constraints: full visibility, all axes, holes, concavity, detached parts and crossing edges passed');
