import json
from pathlib import Path
import sqlite3
from contextlib import closing
import tempfile
import unittest
from relocate_data import relocate, seed


class MigrationTests(unittest.TestCase):
    def test_rebase_preserves_rows_and_nested_json(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            (root / 'images').mkdir()
            (root / 'images/a.tif').write_bytes(b'test')
            old = 'D:\\old\\backend-go\\data\\images\\a.tif'
            with closing(sqlite3.connect(root / 'meta.db')) as db, db:
                db.execute('CREATE TABLE images (id TEXT, file_path TEXT, meta_json TEXT)')
                db.execute('CREATE TABLE tasks (id TEXT)')
                db.execute('CREATE TABLE results (mask_path TEXT)')
                db.execute('INSERT INTO images VALUES (?, ?, ?)', ('original-id', old, json.dumps({'path': old})))
                db.execute('INSERT INTO results VALUES (?)', ('D:/old/backend-go/data/results/missing.tif',))
            report = relocate(root, '/data')
            self.assertEqual(report['changed_cells'], 2)
            self.assertEqual(len(report['missing_paths']), 1)
            with closing(sqlite3.connect(root / 'meta.db')) as db, db:
                row = db.execute('SELECT * FROM images').fetchone()
                self.assertEqual(row[:2], ('original-id', '/data/images/a.tif'))
                self.assertEqual(json.loads(row[2])['path'], '/data/images/a.tif')
            self.assertTrue((root / 'meta.db.pre-migration').exists())
            self.assertEqual(relocate(root, '/data')['changed_cells'], 0)

    def test_seed_once_and_refuse_nonempty_volume(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            source = root / 'source'
            source.mkdir()
            (source / 'payload').write_text('original')
            destination = root / 'destination'
            seed(source, destination)
            (destination / 'payload').write_text('updated')
            seed(source, destination)
            self.assertEqual((destination / 'payload').read_text(), 'updated')
            unsafe = root / 'unsafe'
            unsafe.mkdir()
            (unsafe / 'existing').touch()
            with self.assertRaises(RuntimeError):
                seed(source, unsafe)


if __name__ == '__main__':
    unittest.main()
