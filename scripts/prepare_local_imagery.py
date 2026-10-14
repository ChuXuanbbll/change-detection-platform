"""Build local overview indexes; optional source folder verifies the import hashes.
Usage: python scripts/prepare_local_imagery.py [original_folder]
Never modifies files outside the project's data/imagery directory.
"""
import hashlib
import json
from pathlib import Path
import sys
import rasterio
from rasterio.enums import Resampling

root = Path(__file__).resolve().parents[1]
folder = root / 'data/imagery'
manifest = folder / 'import-checksums.json'
prior = {r['file']: r for r in json.loads(manifest.read_text(encoding='utf-8'))} if manifest.exists() else {}
source = Path(sys.argv[1]) if len(sys.argv) > 1 else None

def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()

records = []
for path in sorted(folder.glob('*.tif')):
    record = dict(prior.get(path.name, {'file': path.name}))
    current = digest(path)
    if source:
        expected = digest(source / path.name)
        if current != expected and not (record.get('original_sha256') == expected and record.get('prepared_sha256') == current):
            raise ValueError(f'Import checksum mismatch: {path.name}')
        record['original_sha256'] = expected
    elif record.get('prepared_sha256') and record['prepared_sha256'] != current:
        raise ValueError(f'Prepared TIFF changed: {path.name}')
    with rasterio.open(path) as src:
        has_overviews = bool(src.overviews(1))
    if not has_overviews:
        with rasterio.open(path, 'r+') as dst:
            dst.build_overviews([2,4,8,16,32,64], Resampling.average)
            dst.update_tags(ns='rio_overview', resampling='average')
    record['prepared_sha256'] = digest(path) if not has_overviews else current
    records.append(record)
    print('Ready:', path.name, flush=True)
manifest.write_text(json.dumps(records,ensure_ascii=False,indent=2),encoding='utf-8')
