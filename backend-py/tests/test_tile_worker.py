import asyncio
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch, MagicMock
import numpy as np
import rasterio
from rasterio.transform import from_bounds
from rasterio.warp import transform_bounds
from app.fetchers import local, regions
from app.utils.tile_worker import DatasetCache, TileJob, prepare_job
from app.utils.tile_queue import TileRenderer
from test_tile_queue import Connection


def fail_process():
    os._exit(31)


class DatasetCacheTests(unittest.TestCase):
    def test_source_paths_reuse_validation_and_invalidate_with_version_root_and_generation(self):
        cache = DatasetCache(2)
        row = {'source_root': 'a', 'file': 'scene.tif', 'version': 'v1'}
        with patch.object(local, 'source_path', return_value=Path('validated.tif')) as resolve:
            self.assertEqual(cache.source_path(row), Path('validated.tif'))
            cache.source_path(row)
            self.assertEqual(resolve.call_count, 1)
            cache.source_path({**row, 'version': 'v2'})
            cache.source_path({**row, 'source_root': 'b'})
            self.assertEqual(resolve.call_count, 3)
            self.assertEqual(len(cache.paths), 2)
            cache.source_path(row)  # The old version was evicted.
            self.assertEqual(resolve.call_count, 4)
            cache.close()
            cache.source_path(row)
            self.assertEqual(resolve.call_count, 5)

    def test_source_path_validation_failures_are_not_cached(self):
        cache = DatasetCache()
        with tempfile.TemporaryDirectory() as root:
            row = {'source_root': root, 'file': '../escape.tif', 'version': 'v1'}
            for _ in range(2):
                with self.assertRaisesRegex(ValueError, '影像路径越界'):
                    cache.source_path(row)
            self.assertEqual(len(cache.paths), 0)

    def test_reuse_eviction_version_change_failure_and_close(self):
        opened=[]
        def make(*args,**kwargs):
            h=MagicMock();opened.append(h);return h
        cache=DatasetCache(2)
        with patch('rasterio.open',side_effect=make):
            with cache.open('a','v1',overview_level=-1) as a:pass
            with cache.open('a','v1',overview_level=-1) as reused:self.assertIs(a,reused)
            with cache.open('b','v1'):pass
            with cache.open('c','v1'):pass
            a.close.assert_called_once()
            with cache.open('b','v2'):pass
            opened[1].close.assert_called_once()
            with self.assertRaises(OSError):
                with cache.open('b','v2'):raise OSError('broken read')
            opened[3].close.assert_called_once()
            with cache.open('b','v2'):pass
            cache.close()
            self.assertEqual(len(opened),5)
            self.assertTrue(all(h.close.call_count==1 for h in opened))


class TileWorkerTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.path=Path(self.temp.name)
        extent=20037508.342789244;step=2*extent/2**16
        self.bounds=(-extent+53868*step,extent-25071*step,-extent+53872*step,extent-25067*step)
        self.rows=[]
        for version,color in [('v1',80),('v2',170)]:
            name=f'{version}.tif'
            with rasterio.open(self.path/name,'w',driver='GTiff',width=1024,height=1024,count=3,
                dtype='uint8',crs='EPSG:3857',transform=from_bounds(*self.bounds,1024,1024),tiled=True) as dst:
                dst.write(np.full((3,1024,1024),color,dtype='uint8'))
            self.rows.append({'id':'test','file':name,'region_id':'xiongan','date':'2025-01-01',
                'version':version,'available':True,'resolution_m':2,
                'bounds':list(transform_bounds('EPSG:3857','EPSG:4326',*self.bounds))})
        self.renderer=TileRenderer(concurrency=2,wait_seconds=1)
        self.addAsyncCleanup(self.renderer.close)

    def job(self,row=0,generation=0):
        return TileJob((self.rows[row],),'xiongan',16,53870,25068,
                       (str(self.path),str(local.BOUNDARY),str(regions.REGIONS),generation))

    async def test_spawn_pixels_cache_version_refresh_and_shutdown(self):
        await self.renderer.start()
        self.assertEqual(len(self.renderer.pool._processes),2)
        for index,generation in [(0,0),(1,0),(1,1)]:
            with patch.object(local,'IMAGERY',self.path):
                expected=local._render_tile_rows([self.rows[index]],'xiongan',16,53870,25068)
            response=await self.renderer.render(Connection(),self.job(index,generation))
            self.assertEqual(response.body,expected)
            self.assertEqual(response.headers['x-tile-cache'],'miss')
            self.assertEqual((await self.renderer.render(Connection(),self.job(index,generation))).headers['x-tile-cache'],'hit')
        processes=list(self.renderer.pool._processes.values())
        await self.renderer.close()
        self.assertTrue(all(not p.is_alive() for p in processes))

    async def test_dead_worker_pool_is_rebuilt(self):
        from concurrent.futures.process import BrokenProcessPool
        from fastapi import HTTPException
        await self.renderer.start()
        with self.assertRaises(BrokenProcessPool):
            await asyncio.wrap_future(self.renderer.pool.submit(fail_process))
        with self.assertRaises(HTTPException) as error:
            await self.renderer.render(Connection(),self.job())
        self.assertEqual(error.exception.status_code,503)
        self.assertEqual((await self.renderer.render(Connection(),self.job())).status_code,200)

    async def test_parent_rejects_stale_before_cached_response(self):
        from app.routers.wayback import _render_http
        from fastapi import HTTPException
        from functools import partial
        from types import SimpleNamespace
        req=Connection();req.app=SimpleNamespace(state=SimpleNamespace(tile_renderer=self.renderer))
        rows=[self.rows[0]];version=local.mosaic_version(rows)
        prepare=partial(prepare_job,'mosaic','xiongan',None,['test'],16,53870,25068,version)
        with patch.object(local,'catalog',side_effect=lambda:rows),patch.object(local,'IMAGERY',self.path):
            self.assertEqual((await _render_http(req,prepare,'no-cache')).status_code,200)
            rows[:]=[self.rows[1]]
            with self.assertRaises(HTTPException) as error:await _render_http(req,prepare,'no-cache')
            self.assertEqual(error.exception.status_code,409)
            self.assertEqual(error.exception.headers['Cache-Control'],'no-store')
