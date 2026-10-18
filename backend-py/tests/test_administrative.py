"""County references preserve every component and hole through detection masks."""
import json
import os
import sqlite3
import unittest
from contextlib import closing
from unittest.mock import patch

import numpy as np
import rasterio
from rasterio.features import geometry_mask
from shapely.geometry import MultiPolygon, Polygon, box, mapping

from app.fetchers import local
from app.fetchers.administrative import boundary_feature
from app.routers.wayback import LocalFetchRequest, fetch_local, list_region_scenes
from tests import test_polygon_selection as polygon_tests


class AdministrativeSelectionTests(unittest.TestCase):
    def setUp(self):
        polygon_tests.PolygonSelectionTests.setUp(self)
        outer = [(114.301, 38.101), (114.306, 38.101), (114.306, 38.109), (114.301, 38.109)]
        hole = [(114.302, 38.103), (114.304, 38.103), (114.304, 38.106), (114.302, 38.106)]
        geometry = MultiPolygon([Polygon(outer, [hole]), box(114.308, 38.102, 114.309, 38.108)])
        self.county = json.loads(json.dumps(mapping(geometry)))
        feature = {'type': 'Feature', 'id': '130629', 'geometry': self.county, 'properties': {
            'admin_code': '130629', 'name': '测试县', 'level': 'county', 'source_id': 'test',
            'bounds': list(geometry.bounds), 'boundary_version': 'fixture-v1'}}
        with closing(sqlite3.connect(self.root / 'meta.db')) as conn:
            conn.executescript('''CREATE TABLE administrative_datasets(id TEXT PRIMARY KEY, active INTEGER);
                CREATE TABLE administrative_regions(dataset_id TEXT, admin_code TEXT, document TEXT);''')
            conn.execute('INSERT INTO administrative_datasets VALUES(?,1)', ('fixture-v1',))
            conn.execute('INSERT INTO administrative_regions VALUES(?,?,?)', ('fixture-v1', '130629', json.dumps(feature)))
            conn.commit()
        self.environment = patch.dict(os.environ, {'DATA_DIR': str(self.root)})
        self.environment.start()

    def tearDown(self):
        self.environment.stop()
        polygon_tests.PolygonSelectionTests.tearDown(self)

    def crop(self, aoi=None, selected=True):
        return fetch_local(LocalFetchRequest(bounds=self.bounds, region_id='test', scene_ids=['a', 'b'],
            aoi=self.county if aoi is None else aoi, admin_code='130629', admin_version='fixture-v1',
            output_dir=str(self.root / 'county-crop')))

    # Reuse the existing meaningful alignment/detection/statistics/thumbnail
    # test with this trusted MultiPolygon instead of a hand-drawn triangle.
    def test_county_holes_and_detached_part_survive_crop_and_detection(self):
        result = self.crop()
        self.assertEqual(result['aoi'], self.county)
        self.assertEqual(result['admin_version'], 'fixture-v1')
        for item in result['images']:
            with rasterio.open(item['path']) as src:
                valid = src.dataset_mask() > 0
                np.testing.assert_array_equal(valid, geometry_mask([self.county], src.shape, src.transform, invert=True))
                for lng, lat, expected in [(114.303, 38.104, False), (114.307, 38.104, False), (114.3085, 38.104, True)]:
                    row, col = src.index(lng, lat)
                    self.assertEqual(bool(valid[row, col]), expected)
        polygon_tests.PolygonSelectionTests.test_alignment_results_statistics_and_transparent_thumbnail(self)

    def test_unknown_version_source_and_bbox_substitution_rejected(self):
        for args in [('130629', 'unknown', 'test'), ('130629', 'fixture-v1', 'other'), ('130630', None, 'test')]:
            with self.subTest(args=args), self.assertRaises(ValueError):
                boundary_feature(*args)
        with self.assertRaisesRegex(ValueError, '不一致'):
            local.selection(self.bounds, mapping(box(*self.bounds)), 'test', '130629', 'fixture-v1')
        feature = boundary_feature('130629', source_id='test')
        self.assertEqual(feature['geometry'], self.county)

    def test_candidate_filter_preserves_full_phase_and_mosaic_version(self):
        outside = {**self.rows[0], 'id': 'outside', 'bounds': [114.32, 38.1, 114.33, 38.11]}
        rows = [*self.rows, outside]
        with patch.object(local, 'catalog', return_value=rows), \
             patch('app.routers.wayback.catalog', return_value=rows), \
             patch('app.routers.wayback.region_feature', return_value={'properties': {'name': '来源'}}):
            unfiltered = list_region_scenes('test')
            filtered = list_region_scenes('test', admin_code='130629')
            self.assertEqual([row['id'] for row in filtered['scenes']], ['a', 'b'])
            self.assertEqual(filtered['phases'][0]['matching_scene_count'], 1)
            self.assertEqual(filtered['phases'][0]['version'], unfiltered['phases'][0]['version'])
            self.assertEqual(len(filtered['phases'][0]['scenes']), 2)
            local._mosaic_rows('test', ['a', 'outside'], 0, 0, 0, filtered['phases'][0]['version'])
        no_coverage = [{**self.rows[0], 'bounds': outside['bounds']}]
        with patch('app.routers.wayback.catalog', return_value=no_coverage), \
             patch('app.routers.wayback.region_feature', return_value={'properties': {'name': '来源'}}):
            self.assertEqual(list_region_scenes('test', admin_code='130629')['phases'], [])


