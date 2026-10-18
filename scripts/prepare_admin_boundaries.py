"""Snapshot the public Tianditu administrative map API for offline use.

The map service is public; this does not use the separately authenticated export
service. Its wire decoding follows the JavaScript client published on the page.
Run explicitly to update; application startup never downloads external data.
"""
import hashlib
import argparse
import json
import re
from pathlib import Path
import struct
import zlib
from datetime import datetime, timezone

import requests
from shapely.geometry import shape, mapping
from shapely.ops import unary_union

ROOT = Path(__file__).resolve().parents[1] / 'data' / 'boundaries'
BASE = 'https://cloudcenter.tianditu.gov.cn/api/portal/region'
PAGE = 'https://cloudcenter.tianditu.gov.cn/administrativeDivision'
SPECIAL = {'130629': 'xiongan', '130632': 'xiongan', '130638': 'xiongan',
           '130682': '130682', '130181': '130181'}


def validate_dataset(dataset):
    features = dataset['features']
    by_code = {f['id']: f for f in features}
    if len(features) != len(by_code) or len(features) != 179:
        raise ValueError('Snapshot count changed; review the administrative change before publishing')
    for feature in features:
        properties = feature['properties']
        geometry = shape(feature['geometry'])
        if geometry.is_empty or not geometry.is_valid or geometry.geom_type not in ('Polygon', 'MultiPolygon'):
            raise ValueError(f"Invalid geometry: {feature['id']}")
        w, s, e, n = geometry.bounds
        if not (113 < w < e < 120 and 35 < s < n < 43):
            raise ValueError('Geometry is not a Hebei geographic coordinate outline')
        parent = properties['parent_code']
        if parent and parent not in by_code:
            raise ValueError('Unknown parent')
        if properties['level'] == 'county' and properties['source_id'] != SPECIAL.get(feature['id'], parent):
            raise ValueError('Business source membership changed')
    for parent in (f for f in features if f['properties']['level'] != 'county'):
        children = [shape(f['geometry']) for f in features if f['properties']['parent_code'] == parent['id']]
        combined = unary_union(children)
        if combined.symmetric_difference(shape(parent['geometry'])).area > 1e-9:
            raise ValueError(f"Parent/child boundary mismatch: {parent['id']}")
        if sum(child.area for child in children) - combined.area > 1e-9:
            raise ValueError('Administrative areas overlap')


def decode_map(raw):
    unpacked = zlib.decompress(raw, 47)
    return json.loads(bytes((number[0] >> 2) & 255 for number in struct.iter_unpack('>i', unpacked)))


def build_business_views(dataset):
    # Preserve all existing imagery group IDs and their ordering, while deriving
    # their display polygons from this authoritative county snapshot.
    legacy = json.loads((ROOT / 'hebei-regions.geojson').read_text(encoding='utf-8'))
    features = []
    for original in legacy['features']:
        source_id = original['id']
        members = [f for f in dataset['features'] if f['properties']['level'] == 'county' and f['properties']['source_id'] == source_id]
        geometry = unary_union([shape(f['geometry']) for f in members])
        point = geometry.representative_point()
        features.append({'type':'Feature', 'id':source_id, 'geometry':mapping(geometry),
                         'properties':{**original['properties'], 'label':[point.x,point.y], 'admin_codes':[f['id'] for f in members],
                                       'boundary_version':dataset['dataset_id']}})
    collection = {'type':'FeatureCollection', 'coordinate_system':'EPSG:4326',
                  'source':PAGE, 'data_date':dataset['data_date'], 'features':features}
    (ROOT / 'hebei-source-groups.geojson').write_text(json.dumps(collection,ensure_ascii=False,separators=(',',':')),encoding='utf-8')
    province = {'type':'FeatureCollection','features':[dataset['features'][0]],'source':PAGE,'data_date':dataset['data_date']}
    (ROOT / 'hebei-administrative-province.geojson').write_text(json.dumps(province,ensure_ascii=False,separators=(',',':')),encoding='utf-8')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--download', action='store_true', help='Explicitly refresh the public snapshot')
    parser.add_argument('--data-date', help='YYYY-MM declared by the source page, verified before downloading')
    args = parser.parse_args()
    ROOT.mkdir(parents=True, exist_ok=True)
    target = ROOT / 'hebei-administrative.geojson'
    if target.exists() and not args.download:
        dataset = json.loads(target.read_text(encoding='utf-8'))
        validate_dataset(dataset)
        build_business_views(dataset)
        print('Rebuilt imagery-source views from the existing offline snapshot')
        return
    if not args.data_date or not re.fullmatch(r'\d{4}-(0[1-9]|1[0-2])', args.data_date):
        parser.error('--download requires --data-date YYYY-MM verified from the source page')
    sources = []
    session = requests.Session()

    def get(path, params=None):
        response = session.get(BASE + path, params=params, timeout=45)
        response.raise_for_status()
        sources.append({'url': response.url, 'sha256': hashlib.sha256(response.content).hexdigest()})
        return response

    menu = get('/menu').json()
    province = next(p for p in menu['data'][0]['children'] if p['gb'] == '156130000')
    cities = decode_map(get('/map', {'gb': province['gb'], 'level': 1}).content)
    city_shapes = {f['properties']['gb']: f for f in cities['features']}
    if set(city_shapes) != {node['gb'] for node in province['children']}:
        raise ValueError('Province tree/prefecture geometry mismatch')
    features = []

    def feature(node, geometry, level, parent_code, path, source_id):
        geom = shape(geometry)
        if geom.is_empty or not geom.is_valid or geom.geom_type not in ('Polygon', 'MultiPolygon'):
            raise ValueError(f"Invalid official geometry: {node['gb']}")
        label = geom.representative_point()
        code = node['gb'][3:]
        return {'type': 'Feature', 'id': code, 'geometry': geometry, 'properties': {
            'admin_code': code, 'name': node['name'], 'level': level, 'parent_code': parent_code,
            'admin_path': path, 'source_id': source_id, 'label': [label.x, label.y],
            'bounds': list(geom.bounds), 'boundary_version': f'tianditu-{args.data_date}'}}

    for city in province['children']:
        code = city['gb'][3:]
        path = [province['name'], city['name']]
        features.append(feature(city, city_shapes[city['gb']]['geometry'], 'prefecture', '130000', path, code))
        counties = decode_map(get('/map', {'gb': city['gb'], 'level': 0}).content)
        by_code = {f['properties']['gb']: f for f in counties['features']}
        if set(by_code) != {n['gb'] for n in city['children']}:
            raise ValueError(f"Tree/geometry mismatch: {city['name']}")
        for county in city['children']:
            county_code = county['gb'][3:]
            features.append(feature(county, by_code[county['gb']]['geometry'], 'county', code,
                                    path + [county['name']], SPECIAL.get(county_code, code)))
    province_geometry = mapping(unary_union([shape(f['geometry']) for f in features if f['properties']['level'] == 'prefecture']))
    features.insert(0, feature(province, province_geometry, 'province', '', ['河北省'], ''))
    if len(features) != 179 or len({f['id'] for f in features}) != 179:
        raise ValueError('Expected 1 province, 11 prefectures and 167 counties')
    dataset = {'type': 'FeatureCollection', 'schema_version': 1, 'dataset_id': f'tianditu-hebei-{args.data_date}',
               'coordinate_system': 'EPSG:4326', 'source': PAGE, 'publisher': '国家基础地理信息中心 / 天地图',
               'data_date': args.data_date, 'usage': '官方声明仅供地图可视化；检测使用该公开版本轮廓作为空间掩膜，不代表法定勘界或测绘精度。',
               'retrieved_at': datetime.now(timezone.utc).isoformat(), 'sources': sources,
               'notes': ['Coordinates retained from the public geographic map API; no GCJ-02 transform applied.',
                         'Province outline is the union of the 11 prefectures from this same snapshot.',
                         'parent_code records the source navigation hierarchy, including city administration of county-level cities.',
                         'Business source_id mapping is independent of administrative parent_code.'],
               'features': features}
    validate_dataset(dataset)
    target.write_text(json.dumps(dataset, ensure_ascii=False, separators=(',', ':')), encoding='utf-8')
    build_business_views(dataset)
    print(f'{target}: {len(features)} features, {target.stat().st_size} bytes')


if __name__ == '__main__':
    main()
