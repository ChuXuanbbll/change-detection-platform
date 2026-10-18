"""Administrative navigation and declarative display sources, separate from detection crops."""
from functools import lru_cache
import json
from pathlib import Path
from urllib.parse import urlsplit

from shapely.geometry import shape

from app.fetchers import local

REGIONS = local.DATA / 'boundaries/hebei-source-groups.geojson'
MAP_SOURCES = local.DATA / 'imagery/map-sources.json'


def read_versioned(path):
    stat = path.stat()
    return _read(str(path), stat.st_mtime_ns, stat.st_size)


@lru_cache(maxsize=8)
def _read(path, modified, size):
    return json.loads(Path(path).read_text(encoding='utf-8'))


def region_collection():
    try:
        data = read_versioned(REGIONS)
        if data.get('coordinate_system') != 'EPSG:4326' or len(data['features']) != 14:
            raise ValueError('Expected 14 WGS84 regions')
        return data
    except (OSError, ValueError, KeyError, TypeError) as exc:
        raise local.BoundaryUnavailable('影像来源边界缺失或无效，请运行 scripts/prepare_admin_boundaries.py') from exc


def region_feature(region_id):
    for feature in region_collection()['features']:
        if feature['properties']['region_id'] == region_id:
            return feature
    raise KeyError(region_id)


def region_geometry(region_id):
    return shape(region_feature(region_id)['geometry'])


def display_sources(region_id, year=None):
    feature = region_feature(region_id)
    geom = shape(feature['geometry'])
    sources = []
    # Existing scenes remain the source of truth for actual local raster files.
    for row in local.catalog():
        if not row.get('available', True):
            continue
        if row.get('region_id') != region_id or not geom.intersects(local.box(*row['bounds'])):
            continue
        sources.append({'id': row['id'], 'region_id': region_id, 'year': int(row['date'][:4]),
                        'date': row['date'], 'label': f"{row['region']} · {row['date'][:4 if row.get('date_precision') == 'year' else 7]}",
                        'type': 'xyz', 'bounds': row['bounds'], 'min_zoom': 0, 'max_zoom': 20,
                        'url': f"/py/local/regions/{region_id}/tiles/{row['id']}/{{z}}/{{x}}/{{y}}.png",
                        'attribution': '本地遥感影像', '_mosaic_key': row['region']})
    if MAP_SOURCES.exists():
        manifest = read_versioned(MAP_SOURCES)
        if manifest.get('schema_version') != 1 or not isinstance(manifest.get('sources'), list):
            raise ValueError('map-sources.json 必须使用 schema_version=1 和 sources 数组')
        ids = set()
        for source in manifest['sources']:
            if source['id'] in ids:
                raise ValueError('显示源 id 必须唯一')
            ids.add(source['id'])
            region_feature(source['region_id'])
            if not isinstance(source['year'], int) or isinstance(source['year'], bool) or not 1900 <= source['year'] <= 9999:
                raise ValueError('显示源 year 必须是 1900 到 9999 的整数')
            if source['type'] not in ('xyz', 'tms', 'wms'):
                raise ValueError('显示源 type 必须为 xyz、tms 或 wms；栅格请登记到 catalog.json')
            url = source['url']
            parsed = urlsplit(url)
            if not ((url.startswith('/') and not url.startswith('//')) or parsed.scheme in ('http', 'https')):
                raise ValueError('显示源 url 必须为同源路径或 HTTP(S) 地址')
            if source['type'] in ('xyz', 'tms') and not all(token in url for token in ('{z}', '{x}', '{y}')):
                raise ValueError('XYZ/TMS 地址须包含 {z}/{x}/{y}')
            if source['type'] == 'wms' and not source.get('layers'):
                raise ValueError('WMS 必须声明 layers')
            if source.get('crs', 'EPSG:3857') not in ('EPSG:3857', 'EPSG:4326'):
                raise ValueError('显示源 crs 必须为 EPSG:3857 或 EPSG:4326')
            if source['type'] != 'wms' and source.get('crs', 'EPSG:3857') != 'EPSG:3857':
                raise ValueError('XYZ/TMS 必须使用 Web Mercator 瓦片网格')
            bounds = source.get('bounds')
            if bounds is not None and (len(bounds) != 4 or not (-180 <= bounds[0] < bounds[2] <= 180 and -90 <= bounds[1] < bounds[3] <= 90)):
                raise ValueError('显示源 bounds 必须为有效 WGS84 [west,south,east,north]')
            if not 0 <= source.get('min_zoom', 0) <= source.get('max_zoom', 20) <= 22:
                raise ValueError('显示源缩放级别无效')
            if source['region_id'] == region_id:
                sources.append({**source, '_mosaic_key': source.get('mosaic_key', source['id'])})
    years = sorted({source['year'] for source in sources}, reverse=True)
    selected_year = year if year is not None else (years[0] if years else None)
    # A single phase per footprint; several footprints can form a city's mosaic.
    latest = {}
    for source in sorted(sources, key=lambda item: (item.get('date', ''), item['id'])):
        if source['year'] == selected_year:
            latest[source['_mosaic_key']] = {key: value for key, value in source.items() if not key.startswith('_')}
    return {'region_id': region_id, 'name': feature['properties']['name'], 'years': years,
            'selected_year': selected_year, 'sources': list(latest.values())}
