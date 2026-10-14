"""Polygon AOIs retain raster validity through crop, alignment, results and previews."""
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import numpy as np
import rasterio
from fastapi import HTTPException
from PIL import Image
from rasterio.features import geometry_mask
from rasterio.transform import from_bounds
from shapely.geometry import Polygon, box, mapping

from app.fetchers import local
from app.routers.detect import DetectRequest, _align_pair, detect
from app.routers.thumbnail import _render
from app.routers.wayback import LocalFetchRequest, SelectionPixelsRequest, fetch_local, get_selection_pixels


class PolygonSelectionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.bounds = [114.30, 38.10, 114.31, 38.11]
        self.region = box(*self.bounds)
        self.aoi = json.loads(json.dumps(mapping(Polygon([
            (114.30, 38.10), (114.31, 38.10), (114.30, 38.11)]))))
        self.rows = []
        for name, size, month in [('a', 40, '01'), ('b', 80, '03')]:
            path = self.root / f'{name}.tif'
            with rasterio.open(path, 'w', driver='GTiff', width=size, height=size, count=3,
                               dtype='uint8', crs='EPSG:4326', transform=from_bounds(*self.bounds, size, size)) as dst:
                rgb = np.full((3, size, size), 100, dtype='uint8')
                rgb[:, -10:, :10] = 0  # Real black pixels are valid; nodata is not set.
                dst.write(rgb)
            self.rows.append(dict(id=name, file=path.name, date=f'2025-{month}-01', region_id='test',
                                  available=True, bounds=self.bounds, version=name))
        self.mocks = [patch.object(local, 'IMAGERY', self.root),
                      patch.object(local, 'catalog', return_value=self.rows),
                      patch.object(local, 'province', return_value=self.region),
                      patch('app.fetchers.regions.region_feature', return_value={'id': 'test'}),
                      patch('app.fetchers.regions.region_geometry', return_value=self.region)]
        for mock in self.mocks:
            mock.start()
        local._crop_resolution.cache_clear()

    def tearDown(self):
        for mock in reversed(self.mocks):
            mock.stop()
        local._crop_resolution.cache_clear()
        self.temp.cleanup()

    def crop(self, aoi=None, selected=True):
        return local.crop_images(self.bounds, '2025-01-01', '2025-12-31', 0, self.root / 'out',
                                 'test', ['a', 'b'] if selected else None, self.aoi if aoi is None else aoi)

    def test_triangle_masks_and_black_valid_pixels_in_both_crop_paths(self):
        for selected in (True, False):
            with self.subTest(selected=selected):
                result = self.crop(selected=selected)
                self.assertEqual(result['count'], 2)
                for image in result['images']:
                    with rasterio.open(image['path']) as src:
                        expected = geometry_mask([self.aoi], src.shape, src.transform, invert=True)
                        np.testing.assert_array_equal(src.dataset_mask() > 0, expected)
                        self.assertTrue((src.read()[:, ~expected] == 0).all())
                        self.assertTrue((src.read(1)[expected] == 0).any())
                        self.assertTrue(expected.any() and (~expected).any())
                        self.assertIsNone(src.nodata)

    def test_fetch_confirms_only_validated_polygon_geometry(self):
        response = fetch_local(LocalFetchRequest(bounds=self.bounds, region_id='test', scene_ids=['a', 'b'],
                                output_dir=str(self.root / 'confirmed'), aoi=self.aoi))
        self.assertEqual(response['aoi'], self.aoi)
        self.assertEqual(response['count'], 2)
        rectangle = fetch_local(LocalFetchRequest(bounds=self.bounds, region_id='test', scene_ids=['a', 'b'],
                                 output_dir=str(self.root / 'rectangle')))
        self.assertNotIn('aoi', rectangle)

    def test_concave_aoi_uses_geometry_bounds_and_preview_matches_crop(self):
        geom = Polygon([(114.301, 38.101), (114.309, 38.101), (114.309, 38.104),
                        (114.304, 38.104), (114.304, 38.109), (114.301, 38.109)])
        aoi = json.loads(json.dumps(mapping(geom)))
        result = self.crop(aoi)
        for source, image in zip(self.rows, result['images']):
            preview = get_selection_pixels(SelectionPixelsRequest(bounds=[0, 0, 1, 1], region_id='test', scene_ids=[source['id']], aoi=aoi))
            self.assertEqual(preview['width'], image['width'])
            self.assertEqual(preview['height'], image['height'])
            self.assertEqual(preview['pixels'], image['width'] * image['height'])
            with rasterio.open(image['path']) as src:
                np.testing.assert_array_equal(src.dataset_mask() > 0,
                    geometry_mask([aoi], src.shape, src.transform, invert=True))
                np.testing.assert_allclose(tuple(src.bounds), geom.bounds)

    def test_invalid_geometries_return_400_without_reading_source_pixels(self):
        w, s, e, n = self.bounds
        invalid = [[], {'type': 'Point', 'coordinates': [w, s]},
                   {'type': 'Polygon', 'coordinates': []},
                   {'type': 'Polygon', 'coordinates': [[[w, s], [e, s], [w, n]]]},
                   {'type': 'Polygon', 'coordinates': [[[w, s], [e, n], [w, n], [e, s], [w, s]]]},
                   {'type': 'Polygon', 'coordinates': [[[w, s], [w+.001, s], [e, s], [w, s]]]},
                   {'type': 'Polygon', 'coordinates': [[[w, s], [e+.001, s], [w, n], [w, s]]]},
                   {'type': 'Polygon', 'coordinates': [self.aoi['coordinates'][0], self.aoi['coordinates'][0]]},
                   {'type': 'Polygon', 'coordinates': [[[w, s], [True, s], [w, n], [w, s]]]},
                   {'type': 'Polygon', 'coordinates': [[[w, s], [float('nan'), s], [w, n], [w, s]]]}]
        with patch.object(local, 'read_mosaic', side_effect=AssertionError('must reject before raster reads')):
            for aoi in invalid:
                with self.subTest(aoi=aoi), self.assertRaises(HTTPException) as raised:
                    fetch_local(LocalFetchRequest(bounds=self.bounds, region_id='test', scene_ids=['a'],
                                                 aoi=aoi, output_dir=str(self.root / 'out')))
                self.assertEqual(raised.exception.status_code, 400)
        with self.assertRaisesRegex(ValueError, 'region_id'):
            local.selection(self.bounds, self.aoi)

    def test_enclosed_administrative_hole_is_rejected(self):
        region = Polygon(self.region.exterior.coords, [[(114.302, 38.102), (114.303, 38.102),
                                                      (114.303, 38.103), (114.302, 38.103)]])
        with patch('app.fetchers.regions.region_geometry', return_value=region):
            with self.assertRaisesRegex(ValueError, '孔洞'):
                self.crop()

    def test_numerical_boundary_tolerance_only(self):
        aoi = json.loads(json.dumps(self.aoi))
        aoi['coordinates'][0][1][0] += 1e-10
        self.assertFalse(local.selection(self.bounds, aoi, 'test').is_empty)
        aoi['coordinates'][0][1][0] += 1e-7
        with self.assertRaisesRegex(ValueError, '行政区'):
            local.selection(self.bounds, aoi, 'test')

    def test_alignment_results_statistics_and_transparent_thumbnail(self):
        images = self.crop()['images']
        out = self.root / 'detection'
        out.mkdir()
        before, after = _align_pair(images[0]['path'], images[1]['path'], out)
        with rasterio.open(before) as b, rasterio.open(after) as a:
            np.testing.assert_array_equal(b.dataset_mask(), a.dataset_mask())
            expected = (b.dataset_mask() > 0) & (a.dataset_mask() > 0)

        class FakeDetector:
            name = 'hebei_onnx'

            def detect(self, before_path, after_path, params, output_dir):
                with rasterio.open(before_path) as src:
                    profile = src.profile.copy()
                profile.update(count=1, dtype='uint8', nodata=None)
                # Deliberately predict changes everywhere, including invalid pixels.
                with rasterio.open(output_dir / 'hebei_onnx_mask.tif', 'w', **profile) as dst:
                    dst.write(np.ones(expected.shape, dtype='uint8'), 1)
                profile.update(dtype='float32')
                with rasterio.open(output_dir / 'hebei_onnx_confidence.tif', 'w', **profile) as dst:
                    dst.write(np.full(expected.shape, .8, dtype='float32'), 1)

        with patch('app.routers.detect.get', return_value=FakeDetector()):
            result = detect(DetectRequest(before_path=before, after_path=after, output_dir=str(out),
                                          params={'kernel_size': 1, 'min_area_pixels': 1}))
        for key in ('mask_path', 'confidence_path'):
            with rasterio.open(result[key]) as src:
                np.testing.assert_array_equal(src.dataset_mask() > 0, expected)
                self.assertTrue((src.read(1)[~expected] == 0).all())
        stats = result['stats']
        self.assertEqual(stats['total_pixels'], int(expected.sum()))
        self.assertEqual(stats['changed_pixels'], int(expected.sum()))
        self.assertEqual(stats['change_ratio'], 1)
        self.assertEqual(stats['confidence_histograms']['all_valid_sample_count'], int(expected.sum()))
        collection = json.loads(Path(result['geojson_path']).read_text())
        self.assertEqual(sum(f['properties']['area_pixels'] for f in collection['features']), int(expected.sum()))
        thumb = self.root / 'thumb.png'
        _render(Path(before), thumb, 256)
        rgba = np.asarray(Image.open(thumb))
        np.testing.assert_array_equal(rgba[:, :, 3] > 0, expected)

    def test_same_grid_without_common_valid_pixels_fails_before_inference(self):
        paths = [self.root / 'x.tif', self.root / 'y.tif']
        profile = dict(driver='GTiff', width=10, height=10, count=3, dtype='uint8', crs='EPSG:4326',
                       transform=from_bounds(*self.bounds, 10, 10))
        for i, path in enumerate(paths):
            with rasterio.open(path, 'w', **profile) as dst:
                dst.write(np.full((3, 10, 10), 100, dtype='uint8'))
                valid = np.zeros((10, 10), dtype='uint8')
                valid[:, i*5:(i+1)*5] = 255
                dst.write_mask(valid)
        with patch('app.routers.detect.get') as get:
            with self.assertRaises(HTTPException) as raised:
                detect(DetectRequest(before_path=str(paths[0]), after_path=str(paths[1]), output_dir=str(self.root / 'empty')))
            self.assertEqual(raised.exception.status_code, 400)
            get.return_value.detect.assert_not_called()

    def test_zero_threshold_does_not_join_components_through_invalid_pixels(self):
        from app.postprocess.pipeline import postprocess
        valid = np.zeros((5, 5), dtype=bool)
        valid[1, 1] = valid[3, 3] = True
        result = postprocess(np.zeros((5, 5), dtype='uint8'), np.zeros((5, 5)),
                             {'threshold': 0, 'kernel_size': 1, 'min_area_pixels': 2}, valid_mask=valid)
        self.assertFalse(result.any())


if __name__ == '__main__':
    unittest.main()
