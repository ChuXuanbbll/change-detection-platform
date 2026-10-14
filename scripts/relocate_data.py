"""Rebase saved file paths after a move; never modify the source database.

Run while services are stopped. Only existing files under the supplied data root
are remapped. Unknown/missing paths are reported, not silently discarded.
"""
import argparse
import json
from pathlib import Path
import shutil
import sqlite3
from contextlib import closing


def relocate(root, target):
    root = Path(root).resolve()
    db = root / 'meta.db'
    if not db.exists():
        return {'database': str(db), 'changed_cells': 0, 'missing_paths': []}
    missing = set()
    count = 0

    def rewrite(value):
        if not isinstance(value, str):
            return value
        normalized = value.replace('\\', '/')
        for marker in ('/backend-go/data/', '/data/validation/api/'):
            if marker in normalized:
                relative = normalized.split(marker, 1)[1]
                candidate = (root / relative).resolve()
                if candidate.is_relative_to(root) and candidate.is_file():
                    return target.rstrip('/\\') + '/' + relative
                missing.add(value)
                return value
        try:
            parsed = json.loads(value)
        except (ValueError, TypeError):
            return value
        if isinstance(parsed, (dict, list)):
            def walk(item):
                if isinstance(item, dict):
                    return {k: walk(v) for k, v in item.items()}
                if isinstance(item, list):
                    return [walk(v) for v in item]
                return rewrite(item)
            updated = walk(parsed)
            if updated != parsed:
                return json.dumps(updated, ensure_ascii=False)
        return value

    backup = db.with_name('meta.db.pre-migration')
    with closing(sqlite3.connect(db)) as connection, connection:
        if not backup.exists():
            with closing(sqlite3.connect(backup)) as saved:
                connection.backup(saved)
        if connection.execute('PRAGMA integrity_check').fetchone()[0] != 'ok':
            raise RuntimeError('SQLite integrity check failed')
        for table in ('images', 'tasks', 'results'):
            columns = [r[1] for r in connection.execute(f'PRAGMA table_info({table})')]
            for row in connection.execute(f'SELECT rowid, * FROM {table}').fetchall():
                for column, old in zip(columns, row[1:]):
                    new = rewrite(old)
                    if new != old:
                        connection.execute(f'UPDATE {table} SET "{column}"=? WHERE rowid=?', (new, row[0]))
                        count += 1
    return {'database': str(db), 'changed_cells': count, 'missing_paths': sorted(missing)}


def seed(source, destination):
    source, destination = Path(source), Path(destination)
    marker = destination / '.seed-complete'
    if marker.exists():
        return
    # Refuse incomplete or foreign volumes rather than overwrite user data.
    if destination.exists() and any(destination.iterdir()):
        raise RuntimeError('Uninitialized volume is not empty; inspect it before retrying')
    shutil.copytree(source, destination, dirs_exist_ok=True)
    report = relocate(destination, '/data')
    (destination / 'migration-report.json').write_text(json.dumps(report, indent=2), encoding='utf-8')
    marker.write_text('initialized\n', encoding='utf-8')
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('data_root')
    parser.add_argument('--target')
    parser.add_argument('--seed-from')
    args = parser.parse_args()
    if args.seed_from:
        seed(args.seed_from, args.data_root)
    else:
        print(json.dumps(relocate(args.data_root, args.target or str(Path(args.data_root).resolve())), indent=2))
