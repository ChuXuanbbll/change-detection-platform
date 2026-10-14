import hashlib
import io
import json
import math
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
from PIL import Image
from shapely.geometry import Point, shape
from shapely.ops import unary_union
from fastapi import HTTPException

from app.fetchers import local, regions
from app.routers.wayback import list_regions, list_region_imagery, region_tile


class RegionTests(unittest.TestCase):
    def setUp(self):
        self.imagery = patch.object(local, 'IMAGERY', local.DATA / 'imagery')
        self.imagery.start()
        local.catalog.cache_clear()
        self.addCleanup(self.imagery.stop)
        self.addCleanup(local.catalog.cache_clear)

    def test_all_14_regions_partition_province_and_labels_are_inside(self):
        data = regions.region_collection()
        expected = {'保定市', '沧州市', '承德市', '定州市', '邯郸市', '衡水市', '廊坊市',
                    '秦皇岛市', '石家庄市', '唐山市', '辛集市', '邢台市', '雄安新区', '张家口市'}
        self.assertEqual({f['properties']['name'] for f in data['features']}, expected)
        geometries = []
        for feature in data['features']:
            geom = shape(feature['geometry'])
            self.assertTrue(geom.is_valid)
            self.assertTrue(geom.covers(Point(*feature['properties']['label'])))
            geometries.append(geom)
        combined = unary_union(geometries)
        self.assertLess(sum(geom.area for geom in geometries) - combined.area, 1e-9)
        self.assertLess(combined.symmetric_difference(local.province()).area, 1e-9)
        metadata = json.loads((regions.REGIONS.parent / 'regions-provenance.json').read_text(encoding='utf-8'))
        for source in metadata['sources']:
            raw = (regions.REGIONS.parent / source['file']).read_bytes()
            self.assertEqual(hashlib.sha256(raw).hexdigest(), source['sha256'])

    def test_independent_regions_and_langfang_exclave(self):
        for child, parent in [('130682', '130600'), ('xiongan', '130600'), ('130181', '130100')]:
            small, large = regions.region_geometry(child), regions.region_geometry(parent)
            self.assertLess(small.intersection(large).area, 1e-10)
            self.assertFalse(large.covers(small.representative_point()))
        self.assertTrue(regions.region_geometry('131000').covers(Point(117.07, 39.98)))

    def test_boundaries_do_not_depend_on_raster_catalog(self):
        with patch.object(local, 'catalog', side_effect=OSError('raster unavailable')):
            self.assertEqual(len(list_regions()['features']), 14)

    def test_no_data_and_unknown_region(self):
        data = list_region_imagery('130600', None)
        self.assertEqual(data['sources'], [])
        self.assertIsNone(data['selected_year'])
        with self.assertRaises(HTTPException) as error:
            list_region_imagery('unknown', None)
        self.assertEqual(error.exception.status_code, 404)

    def test_local_years_and_region_tile(self):
        data = regions.display_sources('130100')
        self.assertEqual(data['years'], [2026, 2025, 2024, 2023])
        self.assertEqual(data['sources'][0]['id'], 'scene-202605-luquan')
        older = regions.display_sources('130100', 2024)
        self.assertEqual(older['sources'][0]['id'], 'scene-202409-luquan')
        row = next(row for row in local.catalog() if row['id'] == data['sources'][0]['id'])
        w, s, e, n = row['bounds']; lng, lat = (w + e) / 2, (s + n) / 2
        z = 13; x = int((lng + 180) / 360 * 2**z)
        y = int((1 - math.asinh(math.tan(math.radians(lat))) / math.pi) / 2 * 2**z)
        rgba = np.asarray(Image.open(io.BytesIO(region_tile('130100', row['id'], z, x, y).body)))
        self.assertTrue((rgba[:, :, 3] > 0).any())
        with self.assertRaises(HTTPException) as error:
            region_tile('130600', row['id'], z, x, y)
        self.assertEqual(error.exception.status_code, 404)

    def test_xiongan_real_tile_and_single_phase_crop(self):
        data = regions.display_sources('xiongan')
        self.assertEqual(data['years'], [2025])
        self.assertEqual(data['selected_year'], 2025)
        self.assertEqual([source['id'] for source in data['sources']], ['scene-202511-xiongan'])
        self.assertEqual(regions.display_sources('xiongan', 2024)['sources'], [])
        lng, lat = 115.97230398038161, 38.95570219392275
        for z in (9, 13, 17):
            with self.subTest(zoom=z):
                x = int((lng + 180) / 360 * 2**z)
                y = int((1 - math.asinh(math.tan(math.radians(lat))) / math.pi) / 2 * 2**z)
                rgba = np.asarray(Image.open(io.BytesIO(region_tile('xiongan', 'scene-202511-xiongan', z, x, y).body)))
                self.assertTrue((rgba[:, :, 3] > 0).any())
                self.assertTrue((rgba[:, :, :3] > 0).any())
        with tempfile.TemporaryDirectory() as temporary:
            result = local.crop_images([lng-.00025, lat-.00025, lng+.00025, lat+.00025],
                                       '2025-11-15', '2025-11-30', 0, temporary)
            self.assertEqual(result['count'], 1)
            self.assertEqual(result['images'][0]['source_file'], '雄安新区亚米202511.tif')
            import rasterio
            with rasterio.open(result['images'][0]['path']) as src:
                self.assertEqual(src.crs.to_epsg(), 4326)
                self.assertEqual(src.count, 3)
                self.assertTrue((src.dataset_mask() > 0).any())
                self.assertTrue((src.read() > 0).any())

    def test_manifest_formats_refresh_and_phase_selection(self):
        with tempfile.TemporaryDirectory() as temporary:
            manifest = Path(temporary) / 'map-sources.json'
            base = {'region_id': '130600', 'year': 2027, 'url': '/tiles/{z}/{x}/{y}.png'}
            sources = [{**base, 'id': 'xyz', 'type': 'xyz'}, {**base, 'id': 'tms', 'type': 'tms'},
                       {**base, 'id': 'wms', 'type': 'wms', 'url': '/wms', 'layers': 'annual', 'crs': 'EPSG:4326'}]
            manifest.write_text(json.dumps({'schema_version': 1, 'sources': sources}), encoding='utf-8')
            with patch.object(regions, 'MAP_SOURCES', manifest), patch.object(local, 'catalog', return_value=[]):
                self.assertEqual({source['type'] for source in regions.display_sources('130600')['sources']}, {'xyz', 'tms', 'wms'})
                sources.append({**base, 'id': 'new-year', 'type': 'xyz', 'year': 2028})
                manifest.write_text(json.dumps({'schema_version': 1, 'sources': sources}), encoding='utf-8')
                data = regions.display_sources('130600')
                self.assertEqual(data['years'], [2028, 2027])
                self.assertEqual(data['sources'][0]['id'], 'new-year')
                sources[-1]['type'] = 'unsupported'
                manifest.write_text(json.dumps({'schema_version': 1, 'sources': sources}), encoding='utf-8')
                with self.assertRaises(ValueError): regions.display_sources('130600')


if __name__ == '__main__':
    unittest.main()
