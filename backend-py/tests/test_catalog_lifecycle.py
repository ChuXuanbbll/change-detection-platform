"""Directory snapshots: explicit refresh, no timed source IO, and concurrent readers."""
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from threading import Barrier
from tempfile import TemporaryDirectory
import os
import unittest
from unittest.mock import patch

from app.fetchers import local, imagery_index
from app.routers.wayback import list_catalog, list_region_scenes


class CatalogLifecycleTests(unittest.TestCase):
    def setUp(self):
        self.temp = TemporaryDirectory()
        self.root = Path(self.temp.name) / '2025年'
        self.folder = self.root / '1月' / '石家庄市'
        self.folder.mkdir(parents=True)
        (self.folder / 'first.tif').write_bytes(b'fixture')
        self.data = Path(self.temp.name) / 'data'
        feature = {'properties': {'name': '石家庄市', 'region_id': '130100'}}
        self.patches = [
            patch.object(local, 'IMAGERY', self.root),
            patch.object(local, 'DATA', self.data),
            patch('app.fetchers.regions.region_collection', return_value={'features': [feature]}),
            patch.object(imagery_index, 'raster_metadata', return_value={
                'available': True, 'error': '', 'bounds': [114, 38, 115, 39],
                'width': 16, 'height': 16, 'resolution_m': 2, 'overviews': [2, 4]}),
        ]
        for item in self.patches:
            item.start()
        local.catalog.cache_clear()

    def tearDown(self):
        local.catalog.cache_clear()
        for item in reversed(self.patches):
            item.stop()
        self.temp.cleanup()

    def test_snapshot_never_expires_or_touches_source_on_cache_hits(self):
        rows = local.catalog()
        with patch('time.monotonic', return_value=10**12), \
             patch.object(Path, 'resolve', side_effect=AssertionError('unexpected source resolve')), \
             patch.object(Path, 'stat', side_effect=AssertionError('unexpected source stat')), \
             patch.object(imagery_index.os, 'walk', side_effect=AssertionError('unexpected scan')):
            for _ in range(5):
                self.assertIs(local.catalog(), rows)
                self.assertIs(imagery_index.scan_catalog(self.root, self.data / 'cache/imagery', {'石家庄市': '130100'}), rows)

    def test_explicit_refresh_discovers_additions_removals_and_file_versions(self):
        rows = local.catalog()
        first = self.folder / 'first.tif'
        stat = first.stat()
        os.utime(first, ns=(stat.st_atime_ns, stat.st_mtime_ns + 1_000_000))
        (self.folder / 'second.tif').write_bytes(b'new')
        self.assertIs(local.catalog(), rows)
        refreshed = local.catalog(refresh=True)
        self.assertEqual(len(refreshed), 2)
        self.assertNotEqual(rows[0]['version'], refreshed[0]['version'])
        first.unlink()
        self.assertEqual(len(local.catalog()), 2)
        self.assertEqual([r['filename'] for r in local.catalog(refresh=True)], ['second.tif'])

    def test_concurrent_initial_requests_scan_once(self):
        barrier = Barrier(6)
        def read(_):
            barrier.wait(timeout=5)
            return local.catalog()
        with patch.object(imagery_index.os, 'walk', wraps=os.walk) as walk:
            with ThreadPoolExecutor(max_workers=6) as pool:
                rows = list(pool.map(read, range(6)))
            self.assertEqual(walk.call_count, 1)
            self.assertTrue(all(result is rows[0] for result in rows))

    def test_page_refresh_scans_once_then_region_and_tile_reads_reuse_snapshot(self):
        from shapely.geometry import box
        local.catalog()
        (self.folder / 'second.tif').write_bytes(b'new')
        with patch.object(imagery_index.os, 'walk', wraps=os.walk) as walk, \
             patch('app.routers.wayback.province', return_value=box(114, 38, 115, 39)):
            page = list_catalog(refresh=True)
            self.assertEqual(len(page['images']), 2)
            for _ in range(3):
                self.assertEqual(len(list_region_scenes('130100')['scenes']), 2)
                self.assertIs(local.catalog(), page['images'])
            self.assertEqual(walk.call_count, 1)

    def test_failed_refresh_surfaces_error_and_can_be_retried(self):
        local.catalog()
        with patch.object(imagery_index.os, 'walk', side_effect=OSError('share offline')):
            with self.assertRaisesRegex(OSError, 'share offline'):
                local.catalog(refresh=True)
        self.assertEqual(len(local.catalog(refresh=True)), 1)


if __name__ == '__main__':
    unittest.main()
