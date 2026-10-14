"""Regression tests for spatial filtering, resource limits and real local raster IO."""
import io
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
import numpy as np
import rasterio
from rasterio.transform import from_bounds
from PIL import Image
from shapely.geometry import box
from app.fetchers import local

class LocalImageryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.bounds = [114.30,38.10,114.31,38.11]
        self.row = dict(id='test',file='test.tif',date='2024-09-01',region='test',bounds=self.bounds)
        with rasterio.open(self.root/'test.tif','w',driver='GTiff',width=100,height=100,count=3,
                           dtype='uint8',crs='EPSG:4326',transform=from_bounds(*self.bounds,100,100)) as dst:
            dst.write(np.full((3,100,100),123,dtype='uint8'))
        self.patches = [patch.object(local,'IMAGERY',self.root),patch.object(local,'catalog',return_value=[self.row]),patch.object(local,'province',return_value=box(*local.HEBEI_BOUNDS))]
        for p in self.patches: p.start()
        local.render_tile.cache_clear()
        local.province_mercator.cache_clear()
    def tearDown(self):
        for p in reversed(self.patches): p.stop()
        local.render_tile.cache_clear()
        local.province_mercator.cache_clear()
        self.temp.cleanup()
    def crop(self, bounds=None, start='2024-09-15', end='2024-09-30'):
        return local.crop_images(bounds or self.bounds,start,end,1,self.root/'out')
    def test_month_overlap_and_pixel_content(self):
        out=self.crop([114.302,38.102,114.308,38.108])
        self.assertEqual(out['count'],1)
        with rasterio.open(out['images'][0]['path']) as src:
            self.assertEqual(src.crs.to_epsg(),4326)
            self.assertTrue(np.all(src.read() == 123))
            self.assertLess(src.width,100)
            self.assertEqual(src.tags()['date_precision'],'month')
    def test_no_coverage(self):
        self.assertEqual(self.crop([115,38,115.1,38.1])['count'],0)
    def test_no_date(self):
        self.assertEqual(self.crop(start='2023-01-01',end='2023-12-31')['count'],0)
    def test_year_precision_covers_the_full_year_and_is_preserved(self):
        self.row.update(date='2024-01-01', date_precision='year')
        out = self.crop(start='2024-08-01', end='2024-08-31')
        self.assertEqual(out['count'], 1)
        self.assertEqual(out['images'][0]['date_precision'], 'year')
        with rasterio.open(out['images'][0]['path']) as src:
            self.assertEqual(src.tags()['date_precision'], 'year')
    def test_all_phases_without_limit_and_explicit_compatibility_limit(self):
        from app.routers.wayback import LocalFetchRequest, fetch_local
        rows = [{**self.row, 'id': f'scene-{i}', 'date': f'{1990+i}-09-01'} for i in range(25)]
        request = LocalFetchRequest(bounds=self.bounds, output_dir=str(self.root/'all'))
        self.assertEqual(request.max_images, 0)
        with patch.object(local, 'catalog', return_value=rows):
            result = fetch_local(request)
            self.assertEqual(result['count'], 25)
            self.assertEqual(len(result['dates_requested']), 25)
            self.assertEqual(result['images'][0]['source_file'], 'test.tif')
            request.max_images = 4
            request.output_dir = str(self.root/'limited')
            self.assertEqual(fetch_local(request)['count'], 4)
    def test_bad_bounds_and_dates(self):
        for bounds in ([0,0,1,1],[114,38,113,39],[float('nan'),38,115,39]):
            with self.assertRaises(ValueError): self.crop(bounds)
        with self.assertRaises(ValueError): self.crop(start='2025-01-01',end='2024-01-01')
    def test_boundary_mask(self):
        from shapely.geometry import Polygon
        boundary=Polygon([(114.30,38.10),(114.31,38.10),(114.30,38.11)])
        with patch.object(local,'province',return_value=boundary):
            out=self.crop()
        with rasterio.open(out['images'][0]['path']) as src:
            mask=src.dataset_mask()
            self.assertTrue(np.any(mask==0))
            self.assertTrue(np.any(mask==255))
    def test_transparent_empty_tile(self):
        image=np.array(Image.open(io.BytesIO(local.render_tile('latest',7,0,0))))
        self.assertEqual(image.shape,(256,256,4))
        self.assertFalse(image.any())
    def test_invalid_tile(self):
        with self.assertRaises(ValueError): local.render_tile('latest',30,0,0)
        with self.assertRaises(KeyError): local.render_tile('missing',7,0,0)

    def test_alignment_resolutions_and_masks(self):
        from app.routers.detect import _align_pair
        after=self.root/'after.tif'
        with rasterio.open(after,'w',driver='GTiff',width=40,height=40,count=3,
                           dtype='uint8',crs='EPSG:4326',transform=from_bounds(*self.bounds,40,40)) as dst:
            dst.write(np.full((3,40,40),125,dtype='uint8'))
            valid=np.full((40,40),255,dtype='uint8');valid[:10,:]=0
            dst.write_mask(valid)
        paths=_align_pair(str(self.root/'test.tif'),str(after),self.root)
        with rasterio.open(paths[0]) as before, rasterio.open(paths[1]) as after:
            self.assertEqual(before.shape,after.shape)
            self.assertEqual(before.transform,after.transform)
            self.assertTrue(np.array_equal(before.dataset_mask(),after.dataset_mask()))
            self.assertTrue(np.any(before.dataset_mask()==0))
            self.assertTrue(np.any(before.dataset_mask()==255))

    def test_overlay_preserves_transparency(self):
        from app.routers.overlay import _render_rgb_rgba
        from shapely.geometry import Polygon
        boundary=Polygon([(114.30,38.10),(114.31,38.10),(114.30,38.11)])
        with patch.object(local,'province',return_value=boundary):
            out=self.crop()
        rgba,_,_=_render_rgb_rgba(Path(out['images'][0]['path']))
        self.assertTrue(np.any(rgba[:,:,3]==0))
        self.assertTrue(np.any(rgba[:,:,3]==255))

if __name__ == '__main__': unittest.main()
