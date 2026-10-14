"""Read-only raster discovery; metadata caches live in the project, never on the source drive."""
import hashlib
import json
import os
from pathlib import Path
import re
from threading import RLock

import rasterio
from rasterio.vrt import WarpedVRT
from rasterio.warp import transform_bounds

_lock = RLock()
_cached = {}
INDEX_VERSION = 2


def clear_cache():
    with _lock:
        _cached.clear()


def capture_month(path, root, explicit_year=None):
    year_match = re.search(r'(?:19|20)\d{2}', root.name)
    year = str(explicit_year) if explicit_year else (year_match.group() if year_match else None)
    folder = next((part for part in path.relative_to(root).parts[:-1]
                   if re.fullmatch(r'(1[0-2]|0?[1-9])月', part)), None)
    if not year or not folder:
        raise ValueError('无法从年份目录及月份目录解析时相')
    return f'{year}-{int(folder[:-1]):02d}-01', '月份目录', ''


def raster_metadata(path):
    with rasterio.Env(GDAL_PAM_ENABLED='NO'):
        with rasterio.open(path) as src:
            if src.crs is None:
                raise ValueError('影像缺少坐标系')
            if src.count < 3 or any(dtype != 'uint8' for dtype in src.dtypes[:3]):
                raise ValueError('展示需要至少三个 uint8 RGB 波段')
            bounds = list(transform_bounds(src.crs, 'EPSG:4326', *src.bounds))
            if src.crs.is_projected:
                resolution_m = max(src.res) * src.crs.linear_units_factor[1]
            else:
                with WarpedVRT(src, crs='EPSG:3857') as vrt:
                    resolution_m = max(vrt.res)
            return dict(bounds=bounds, width=src.width, height=src.height,
                        crs=str(src.crs), resolution=list(src.res),
                        resolution_m=round(resolution_m, 6), overviews=src.overviews(1),
                        available=True, error='')


def scan_catalog(root, cache_dir, region_names, explicit_year=None):
    # A cached index must not touch the network drive, even to resolve its root.
    memory_key = (os.path.normcase(os.path.abspath(root)), INDEX_VERSION, explicit_year)
    with _lock:
        if memory_key in _cached:
            return _cached[memory_key]
        root = root.resolve()
        if not root.is_dir():
            raise OSError(f'影像目录不可访问: {root}')
        key = str(root)
        cache_file = cache_dir / (hashlib.sha256(key.encode()).hexdigest()[:20] + '.json')
        try:
            document = json.loads(cache_file.read_text(encoding='utf-8'))
            # Recompute grouping below on every scan, including old index versions.
            # Reuse unchanged raster metadata so a grouping migration does not reopen terabytes of imagery.
            previous = {row['file']: row for row in document['images']}
        except (OSError, ValueError, KeyError, TypeError):
            previous = {}
        rows = []

        def walk_error(error):
            raise error

        for directory, _, files in os.walk(root, onerror=walk_error):
            for filename in files:
                path = Path(directory) / filename
                if path.suffix.lower() not in ('.tif', '.tiff'):
                    continue
                relative = path.relative_to(root).as_posix()
                row = dict(id='scene-' + hashlib.sha256(relative.encode()).hexdigest()[:20],
                           file=relative, filename=filename, date_precision='month', available=False)
                try:
                    if not path.resolve().is_relative_to(root):
                        raise ValueError('影像路径越界')
                    region = next((part for part in path.relative_to(root).parts[:-1] if part in region_names), None)
                    if region is None:
                        raise ValueError('影像目录没有可识别的行政区')
                    row.update(region=region, region_id=region_names[region])
                    row['date'], row['date_source'], row['warning'] = capture_month(path, root, explicit_year)
                    stat = path.stat()
                    row['version'] = f'{stat.st_mtime_ns}:{stat.st_size}'
                    old = previous.get(relative)
                    if old and old.get('version') == row['version'] and old.get('available'):
                        row = {**old, **row, 'available': True}
                    else:
                        row.update(raster_metadata(path))
                except (OSError, ValueError, rasterio.errors.RasterioError) as exc:
                    row.update(available=False, error=str(exc))
                rows.append(row)
        rows.sort(key=lambda row: (row.get('date', '9999'), row['file']))
        cache_dir.mkdir(parents=True, exist_ok=True)
        temporary = cache_file.with_name(f'{cache_file.stem}.{os.getpid()}.tmp')
        temporary.write_text(json.dumps({'index_version': INDEX_VERSION, 'source_root': key, 'images': rows}, ensure_ascii=False), encoding='utf-8')
        temporary.replace(cache_file)
        _cached[memory_key] = rows
        return rows
