"""Serializable raster jobs. Workers load raster dependencies, never the API/model runtime."""
from collections import OrderedDict
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from time import perf_counter
import os


@dataclass(frozen=True)
class TileJob:
    rows: tuple
    region_id: str | None
    z: int
    x: int
    y: int
    context: tuple

    @property
    def key(self):
        return (self.context, tuple(sorted((r['id'], r.get('version', ''), r.get('source_root', ''), r.get('source_priority', 0)) for r in self.rows)),
                self.region_id, self.z, self.x, self.y)


def prepare_job(mode, region_id, scene, scene_ids, z, x, y, version=None):
    from app.fetchers import local, regions
    # Snapshot the generation and validated rows atomically with respect to refresh.
    with local._catalog_lock:
        rows = (local._mosaic_rows(region_id, scene_ids, z, x, y, version) if mode == 'mosaic'
                else local._tile_rows(scene, z, x, y, region_id))
        context = (str(local.IMAGERY), str(local.BOUNDARY), str(regions.REGIONS), local._catalog_generation)
        return TileJob(tuple(dict(row) for row in rows), region_id, z, x, y, context)


class DatasetCache:
    """Owned by one single-task worker; no GDAL handles cross threads/processes."""
    def __init__(self, limit=8):
        self.limit = limit
        self.items = OrderedDict()
        self.paths = OrderedDict()

    def source_path(self, row):
        from app.fetchers import local
        key = (row.get('source_root', str(local.IMAGERY)), row['file'], row.get('version'))
        path = self.paths.get(key)
        if path is None:
            # resolve() performs filesystem IO on Windows/SMB. Validate once per
            # source version and worker generation, just like the dataset handles.
            path = local.source_path(row)
            self.paths[key] = path
            while len(self.paths) > self.limit:
                self.paths.popitem(last=False)
        self.paths.move_to_end(key)
        return path

    @contextmanager
    def open(self, path, version, **options):
        import rasterio
        key = (str(path), version, tuple(sorted(options.items())))
        for old in list(self.items):
            if old[0] == key[0] and old[1] != version:
                self.items.pop(old).close()
        src = self.items.get(key)
        if src is None:
            while len(self.items) >= self.limit:
                self.items.popitem(last=False)[1].close()
            src = self.items[key] = rasterio.open(path, **options)
        self.items.move_to_end(key)
        try:
            yield src
        except BaseException:
            self.items.pop(key, None)
            src.close()
            raise

    def close(self):
        for src in self.items.values():
            src.close()
        self.items.clear()
        self.paths.clear()


_datasets = None
_context = None
_environment = None


def close_worker():
    global _environment
    if _datasets is not None:
        _datasets.close()
    if _environment is not None:
        _environment.__exit__(None, None, None)
        _environment = None


def initialize_worker(handles, cache_mb, barrier):
    global _datasets, _environment
    import rasterio
    from PIL import Image
    from multiprocessing.util import Finalize
    from app.fetchers import local  # noqa: F401 - Warm raster dependencies before accepting work.
    _environment = rasterio.Env(GDAL_PAM_ENABLED='NO', GDAL_CACHEMAX=cache_mb * 1024 * 1024,
                               GDAL_NUM_THREADS='1')
    _environment.__enter__()
    _datasets = DatasetCache(handles)
    Image.init()
    Finalize(None, close_worker, exitpriority=10)
    barrier.wait(timeout=60)


def worker_ready():
    return os.getpid()


def render_job(job):
    global _context
    from app.fetchers import local, regions
    started = perf_counter()
    if _context != job.context:
        _datasets.close()
        local.IMAGERY, local.BOUNDARY, regions.REGIONS = map(Path, job.context[:3])
        local._tile_metadata.cache_clear()
        local.province.cache_clear()
        local.province_mercator.cache_clear()
        regions._read.cache_clear()
        _context = job.context
    data = local._render_tile_rows(list(job.rows), job.region_id, job.z, job.x, job.y, datasets=_datasets)
    return data, (perf_counter() - started) * 1000
