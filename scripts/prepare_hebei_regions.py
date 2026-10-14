"""Build the 14 display regions from cached DataV boundaries in WGS84.

Run with --download once to refresh the three source files; normal builds are offline.
Xiong'an is the union of Xiong, Rongcheng and Anxin counties (display convention).
"""
import argparse
import hashlib
import json
from pathlib import Path
import sys

from shapely import make_valid
from shapely.geometry import mapping, shape
from shapely.ops import transform, unary_union

sys.path.insert(0, str(Path(__file__).resolve().parent))
from prepare_hebei_boundary import inverse, polygons

ROOT = Path(__file__).resolve().parents[1] / 'data/boundaries'
SOURCES = {'cities': '130000', 'baoding': '130600', 'shijiazhuang': '130100'}
ORDER = ['130600', '130900', '130800', '130682', '130400', '131100', '131000',
         '130300', '130100', '130200', '130181', '130500', 'xiongan', '130700']


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--download', action='store_true')
    args = parser.parse_args()
    sources, provenance = {}, []
    for key, code in SOURCES.items():
        path = ROOT / f'hebei-{key}-source.geojson'
        url = f'https://geo.datav.aliyun.com/areas_v3/bound/{code}_full.json'
        if args.download:
            import requests
            response = requests.get(url, timeout=60)
            response.raise_for_status()
            response.json()  # Reject non-JSON responses before replacing the snapshot.
            path.write_bytes(response.content)
        raw = path.read_bytes()
        sources[key] = {str(f['properties']['adcode']): f for f in json.loads(raw)['features']}
        provenance.append({'url': url, 'sha256': hashlib.sha256(raw).hexdigest(), 'file': path.name})
    province = unary_union([shape(f['geometry']) for f in json.loads((ROOT / 'hebei.geojson').read_text(encoding='utf-8'))['features']])

    def normalize(feature):
        return unary_union(polygons(make_valid(transform(inverse, shape(feature['geometry']))))).intersection(province)

    regions = {code: (f['properties']['name'], normalize(f)) for code, f in sources['cities'].items()}
    dingzhou = normalize(sources['baoding']['130682']).intersection(regions['130600'][1])
    xinji = normalize(sources['shijiazhuang']['130181']).intersection(regions['130100'][1])
    xiongan = unary_union([normalize(sources['baoding'][code]) for code in ['130629', '130632', '130638']]).intersection(regions['130600'][1])
    regions['130600'] = ('保定市', regions['130600'][1].difference(unary_union([dingzhou, xiongan])))
    regions['130100'] = ('石家庄市', regions['130100'][1].difference(xinji))
    regions.update({'130682': ('定州市', dingzhou), '130181': ('辛集市', xinji), 'xiongan': ('雄安新区', xiongan)})
    features = []
    for code in ORDER:
        name, geom = regions[code]
        geom = unary_union(polygons(make_valid(geom)))
        if geom.is_empty or not geom.is_valid:
            raise ValueError(f'Invalid region: {name}')
        point = geom.representative_point()
        features.append({'type': 'Feature', 'id': code, 'properties': {
            'region_id': code, 'name': name, 'label': [point.x, point.y],
            'kind': 'new_area' if code == 'xiongan' else ('province_administered_city' if code in ('130682', '130181') else 'prefecture'),
        }, 'geometry': mapping(geom)})
    output = {'type': 'FeatureCollection', 'schema_version': 1, 'coordinate_system': 'EPSG:4326', 'features': features}
    (ROOT / 'hebei-regions.geojson').write_text(json.dumps(output, ensure_ascii=False, separators=(',', ':')), encoding='utf-8')
    metadata = {'sources': provenance, 'coordinate_system': 'EPSG:4326',
                'normalization': 'Inverse GCJ-02, make_valid, clip to bundled province; subtract independent regions from parent cities',
                'xiongan_definition': 'Union of Rongcheng (130629), Anxin (130632), Xiong (130638); not a surveyed planning boundary',
                'note': 'Public visualization boundaries; not survey-grade. Region IDs are stable application keys.'}
    (ROOT / 'regions-provenance.json').write_text(json.dumps(metadata, ensure_ascii=False, indent=2), encoding='utf-8')
    print('Prepared', len(features), 'regions')


if __name__ == '__main__':
    main()
