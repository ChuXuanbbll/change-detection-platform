import L from 'leaflet';
import { provincePolygons, type ProvinceGeometry } from './province';
import { constrainRegionCenter, type ViewExtent, type ViewPolygon } from './regionView';

// Leaflet 1.9 exposes no pre-render center constraint. Keep its private integration
// here, scoped to one map instance, and exercise it with browser drag/zoom tests.
type NavigationMap = L.Map & {
  _mapPane: HTMLElement;
  _limitCenter(center: L.LatLng, zoom: number, bounds?: L.LatLngBounds): L.LatLng;
  _move(center: L.LatLng, zoom: number, ...args: unknown[]): L.Map;
  _rawPanBy(offset: L.Point): void;
  _onPanTransitionStep(): void;
  dragging: L.Handler & { _draggable: L.Evented & { _newPos: L.Point } };
};

export class RegionNavigation {
  private map: NavigationMap;
  private geometry: ProvinceGeometry | null = null;
  private projected: { zoom: number; polygons: ViewPolygon[]; extent: ViewExtent } | null = null;
  private suspended = false;
  private originalLimit: NavigationMap['_limitCenter'];
  private originalMove: NavigationMap['_move'];
  private originalPanStep: NavigationMap['_onPanTransitionStep'];
  private originalRawPan: NavigationMap['_rawPanBy'];

  constructor(map: L.Map) {
    this.map = map as NavigationMap;
    this.originalLimit = this.map._limitCenter;
    this.originalMove = this.map._move;
    this.originalPanStep = this.map._onPanTransitionStep;
    this.originalRawPan = this.map._rawPanBy;
    this.map._limitCenter = (center, zoom, bounds) => this.constrain(this.originalLimit.call(map, center, zoom, bounds), zoom);
    this.map._move = (center, zoom, ...args) => this.originalMove.call(map, this.constrain(center, zoom), zoom, ...args);
    this.map._onPanTransitionStep = () => { this.constrainPane(); this.originalPanStep.call(map); };
    // invalidateSize emits move before resize; constrain the synchronous pan
    // before listeners/renderers see it (also covers non-animated panBy).
    this.map._rawPanBy = offset => { this.originalRawPan.call(map, offset); this.constrainPane(); };
    this.map.dragging._draggable.on('predrag', this.beforeDrag);
    map.on('moveend', this.correct);
  }

  setRegion(geometry: ProvinceGeometry | null): void {
    this.geometry = geometry; this.projected = null;
  }
  suspend(value: boolean): void { this.suspended = value; }

  constrain(center: L.LatLng, zoom = this.map.getZoom()): L.LatLng {
    const map = this.map, size = map.getSize();
    if (!this.geometry || this.suspended || size.x <= 0 || size.y <= 0) return center;
    if (!this.projected || this.projected.zoom !== zoom) {
      const polygons = provincePolygons(this.geometry).map(rings => rings.map(ring => ring.map(([lng, lat]) => map.project([lat, lng], zoom))));
      const bounds = L.bounds(polygons.flat(2));
      this.projected = { zoom, polygons, extent: { min: bounds.min!, max: bounds.max! } };
    }
    const point = constrainRegionCenter(map.project(center, zoom), size, this.projected.extent, this.projected.polygons);
    return map.unproject(L.point(point.x, point.y), zoom);
  }

  private beforeDrag = (): void => {
    if (!this.geometry || this.suspended) return;
    const map = this.map, drag = map.dragging._draggable;
    const pane = L.DomUtil.getPosition(map._mapPane);
    const center = map.project(map.getCenter()).add(pane).subtract(drag._newPos);
    const allowed = map.project(this.constrain(map.unproject(center)));
    drag._newPos = drag._newPos.add(center.subtract(allowed));
  };

  private constrainPane(): void {
    if (!this.geometry || this.suspended) return;
    const map = this.map, center = map.project(map.getCenter());
    const allowed = map.project(this.constrain(map.unproject(center)));
    L.DomUtil.setPosition(map._mapPane, L.DomUtil.getPosition(map._mapPane).add(center.subtract(allowed)));
  }

  correct = (): void => {
    const map = this.map;
    if (!this.geometry || this.suspended || !map.getContainer().clientWidth || !map.getContainer().clientHeight) return;
    const center = this.constrain(map.getCenter());
    if (map.project(center).distanceTo(map.project(map.getCenter())) > 0.5) map.setView(center, map.getZoom(), { animate: false });
  };

  dispose(): void {
    this.map.dragging._draggable.off('predrag', this.beforeDrag);
    this.map.off('moveend', this.correct);
    this.map._limitCenter = this.originalLimit;
    this.map._move = this.originalMove;
    this.map._onPanTransitionStep = this.originalPanStep;
    this.map._rawPanBy = this.originalRawPan;
  }
}
