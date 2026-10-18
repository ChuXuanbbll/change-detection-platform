"""Read the same immutable SQLite boundary snapshots used by Go and the map."""
import json
import os
from contextlib import closing
from pathlib import Path
import sqlite3

from shapely.geometry import shape


def boundary_feature(code, version=None, source_id=None):
    if not isinstance(code, str) or len(code) != 6 or not code.isdigit():
        raise ValueError('行政区代码无效')
    root = Path(os.environ.get('DATA_DIR', str(Path(__file__).resolve().parents[3] / 'backend-go/data')))
    database = root / 'meta.db'
    try:
        with closing(sqlite3.connect(database.resolve().as_uri() + '?mode=ro', uri=True, timeout=5)) as conn:
            row = conn.execute('''SELECT r.document FROM administrative_regions r
                JOIN administrative_datasets d ON d.id=r.dataset_id
                WHERE r.admin_code=? AND ((?='' AND d.active=1) OR r.dataset_id=?)''',
                (code, version or '', version or '')).fetchone()
    except sqlite3.Error as exc:
        from app.fetchers.local import BoundaryUnavailable
        raise BoundaryUnavailable('行政区知识库尚未就绪，请启动或更新 Go 服务') from exc
    if row is None:
        raise ValueError('行政区或边界版本不存在')
    feature = json.loads(row[0])
    properties = feature['properties']
    if properties['level'] != 'county':
        raise ValueError('请选择县级行政区边界')
    if source_id and properties['source_id'] != source_id:
        raise ValueError('县级行政区不属于当前影像来源')
    geometry = shape(feature['geometry'])
    if geometry.is_empty or not geometry.is_valid or geometry.geom_type not in ('Polygon', 'MultiPolygon'):
        raise ValueError('入库行政区边界无效')
    return feature


def boundary_geometry(code, version=None, source_id=None):
    return shape(boundary_feature(code, version, source_id)['geometry'])
