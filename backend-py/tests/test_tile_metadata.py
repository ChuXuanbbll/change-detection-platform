from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import numpy as np
import rasterio
from rasterio.enums import Resampling
from rasterio.transform import from_origin
from app.fetchers import local


class TileMetadataTests(unittest.TestCase):
    def test_versioned_metadata_reused_across_zoom_and_refreshed(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary)/'source.tif'
            with rasterio.open(path, 'w', driver='GTiff', width=512, height=512, count=3,
                               dtype='uint8', crs='EPSG:3857', transform=from_origin(0, 1024, 2, 2)) as dst:
                dst.write(np.full((3,512,512), 127, dtype='uint8'))
                dst.build_overviews([2,4,8], Resampling.nearest)
            local._tile_metadata.cache_clear()
            original = rasterio.open
            with patch.object(local.rasterio, 'open', wraps=original) as opened:
                with ThreadPoolExecutor(max_workers=8) as pool:
                    values = list(pool.map(lambda _: local._tile_overview(path, 8, 'v1'), range(8)))
                self.assertEqual(values, [1]*8)
                self.assertEqual(local._tile_overview(path, 16, 'v1'), 2)
                self.assertEqual(local._tile_overview(path, 1, 'v1'), -1)
                self.assertEqual(opened.call_count, 1, 'one metadata read shared across requests and zooms')
                local._tile_overview(path, 8, 'v2')
                self.assertEqual(opened.call_count, 2, 'a new source version must be reopened')
                local._clear_catalog()
                local._tile_overview(path, 8, 'v2')
                self.assertEqual(opened.call_count, 3, 'explicit refresh also clears sidecar metadata')
            local._tile_metadata.cache_clear()
