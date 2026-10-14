import assert from 'node:assert/strict';
import { constrainToRegion, regionCoversPoint, polygonError, polygonBounds, polygonFromPoints, moveParallelEdge, rectanglePoints, rectangleEditPoints } from '../frontend/src/utils/selectionGeometry.ts';

const polygon = ring => ({ type: 'Polygon', coordinates: [ring] });
const square = polygon([[0, 0], [4, 0], [4, 4], [0, 4], [0, 0]]);
const near = (a, b) => assert.ok(Math.hypot(a[0] - b[0], a[1] - b[1]) < 1e-9, `${a} != ${b}`);
assert.ok(regionCoversPoint(square, [0, 0]));
assert.ok(regionCoversPoint(square, [4, 2]));
assert.ok(!regionCoversPoint(square, [5, 2]));
near(constrainToRegion(square, [2, 2], [6, 2]), [4, 2]);
near(constrainToRegion(square, [4, 2], [6, 2]), [4, 2]);
near(constrainToRegion(square, [0, 0], [4, 0]), [4, 0]);
near(constrainToRegion(square, [2, 2], [6, 6]), [4, 4]);
assert.equal(polygonError([[0, 0], [4, 0], [4, 4], [0, 4]], square), '');
assert.equal(polygonError([[1, 1], [2, 1], [3, 1], [3, 3], [1, 3]], square), '');
assert.ok(polygonError([[0, 0], [4, 4], [0, 4], [4, 0]], square));
assert.ok(polygonError([[0, 0], [1, 0], [2, 0]], square));
assert.ok(polygonError([[0, 0], [4, 0], [2, 0], [2, 2]], square));
assert.ok(polygonError([[0, 0], [4, 0], [0, 0], [0, 4]], square));
const concave = polygon([[0, 0], [4, 0], [4, 4], [3, 4], [3, 1], [1, 1], [1, 4], [0, 4], [0, 0]]);
// Both endpoints inside, but two boundary crossings in the middle.
near(constrainToRegion(concave, [.5, 3], [3.5, 3]), [1, 3]);
assert.ok(polygonError([[.5, .5], [3.5, .5], [3.5, 3], [.5, 3]], concave));
assert.equal(polygonError([[.5, 3], [.5, .5], [3.5, .5], [3.5, 3], [3.25, 3], [3.25, .75], [.75, .75], [.75, 3]], concave), '');
const hole = { ...square, coordinates: [...square.coordinates, [[1, 1], [3, 1], [3, 3], [1, 3], [1, 1]]] };
near(constrainToRegion(hole, [.5, 2], [3.5, 2]), [1, 2]);
assert.ok(regionCoversPoint(hole, [1, 2]));
assert.ok(!regionCoversPoint(hole, [2, 2]));
near(constrainToRegion(hole, [0, 2], [2, 0]), [2, 0]); // Tangency at a hole corner is allowed.
assert.ok(polygonError([[.5, .5], [3.5, .5], [3.5, 3.5], [.5, 3.5]], hole), 'an enclosed hole must be rejected');
const detached = { type: 'MultiPolygon', coordinates: [polygon([[0, 0], [1, 0], [1, 1], [0, 1], [0, 0]]).coordinates,
  polygon([[3, 0], [4, 0], [4, 1], [3, 1], [3, 0]]).coordinates] };
near(constrainToRegion(detached, [.5, .5], [3.5, .5]), [1, .5]);
assert.equal(polygonError([[3, 0], [4, 0], [3.5, .5]], detached), '');
// Snapping to the preceding vertex cannot make an invalid following edge legal.
const edit = [[.5, 3], [.5, .5], [3.5, .5]];
const candidate = constrainToRegion(concave, edit[0], [3.5, 3]);
assert.ok(polygonError([edit[0], candidate, edit[2]], concave));
assert.deepEqual(polygonBounds(polygonFromPoints([[1, 1], [3, 1], [1, 3]])), { west: 1, south: 1, east: 3, north: 3 });
console.log('PASS: boundary snapping, first exit, concavity, holes, detached parts, self intersections, edits and bounds');

const triangle = [[0, 0], [4, 0], [2, 4]];
const shifted = moveParallelEdge(triangle, 0, [10, 1]);
assert.deepEqual(shifted, [[.5, 1], [3.5, 1], [2, 4]], 'endpoints slide on adjacent lines; tangential motion is ignored');
assert.deepEqual(moveParallelEdge(triangle, 0, [0, -2]), [[-1, -2], [5, -2], [2, 4]], 'supporting lines extend outwards');
assert.equal(moveParallelEdge(triangle, 0, [0, 4]), null, 'collapsed edge is rejected');
assert.equal(moveParallelEdge(triangle, 0, [0, 5]), null, 'inverted edge is rejected');
assert.equal(moveParallelEdge([[0, 0], [2, 0], [4, 0], [4, 4], [0, 4]], 0, [0, 1]), null, 'collinear neighbour cannot define an endpoint');
const rectangle = rectanglePoints({ west: 1, south: 1, east: 3, north: 3 });
const shiftedRectangle = moveParallelEdge(rectangle, 0, [2, -.5]);
assert.deepEqual(shiftedRectangle, [[1, .5], [3, .5], [3, 3], [1, 3]]);
assert.equal(polygonError(shiftedRectangle, square), '');
assert.ok(polygonError(moveParallelEdge(rectangle, 0, [0, -2]), square));
assert.deepEqual(moveParallelEdge(rectangle, 3, [-.5, 20]), [[.5, 1], [3, 1], [3, 3], [.5, 3]], 'last edge wraps endpoints');
const clippedBox = rectangleEditPoints({ west: -1, south: 1, east: 2, north: 3 }, square);
assert.equal(polygonError(clippedBox, square), '');
assert.deepEqual(polygonBounds(polygonFromPoints(clippedBox)), { west: 0, south: 1, east: 2, north: 3 });
assert.equal(rectangleEditPoints({ west: 0, south: 2, east: 4, north: 3 }, concave), null, 'a split box cannot become a single polygon');
assert.equal(rectangleEditPoints({ west: 0, south: 0, east: 4, north: 4 }, hole), null, 'do not discard holes when converting rectangles');
assert.equal(rectangleEditPoints({ west: 5, south: 5, east: 6, north: 6 }, square), null);
console.log('PASS: parallel edge movement, adjacent supporting lines, rectangle editing, degeneracy and region validation');
