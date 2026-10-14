import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import rasterio
from rasterio.transform import from_origin

from app.utils.progress import Progress


class ProgressTests(unittest.TestCase):
    def test_atomic_snapshot_and_write_failure_are_nonfatal(self):
        with tempfile.TemporaryDirectory() as directory:
            progress = Progress(directory)
            progress('aligning', 0)
            progress('model_loading', 5)
            data = json.loads(progress.path.read_text())
            self.assertEqual(data, {'stage': 'model_loading', 'progress': 5})
            self.assertFalse(progress.path.with_suffix('.tmp').exists())
            with patch('app.utils.progress.os.replace', side_effect=PermissionError):
                progress('saving', 99)
            self.assertEqual(json.loads(progress.path.read_text()), data)

    def test_real_tile_loop_reports_single_and_partial_batches(self):
        script = Path(__file__).resolve().parents[2] / 'model-runtime/src/predict_city_virtual_patch_pipeline_onnx.py'
        spec = importlib.util.spec_from_file_location('progress_pipeline', script)
        runtime = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(runtime)
        for tiles, expected in [(1, [0, 1]), (5, [0, 2, 4, 5])]:
            with self.subTest(tiles=tiles), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                source = root / 'input.tif'
                with rasterio.open(source, 'w', driver='GTiff', count=3, width=64*tiles, height=64,
                                   dtype='uint8', crs='EPSG:32650', transform=from_origin(500000, 4200000, 1, 1)) as dst:
                    dst.write(np.full((3, 64, 64*tiles), 100, dtype='uint8'))
                command = ['pipeline', '--phase_a', str(source), '--phase_b', str(source),
                           '--out_prob', str(root/'prob.tif'), '--out_mask', str(root/'mask.tif'),
                           '--progress_dir', str(root/'progress'), '--tile_size', '64', '--stride', '64',
                           '--resolution', '1', '--dst_crs', 'EPSG:32650', '--batch_size', '2', '--prefetch_batches', '0']
                with patch('sys.argv', command):
                    args = runtime.parse_args()
                captured = []
                report = runtime.report_worker_progress

                def observe(args, done, total):
                    # Force every batch snapshot to be visible without sleeping.
                    args._progress_write = -100
                    report(args, done, total)
                    snapshot = json.loads((root/'progress/worker-0.json').read_text())
                    captured.append(snapshot['done'])
                    self.assertEqual(snapshot['total'], tiles)

                def predict(model, inputs, *unused):
                    return np.ones((len(inputs),64,64), dtype='float32'), np.ones((len(inputs),64,64), dtype='uint8')

                with patch.object(runtime, 'ONNXChangeDetector') as model, patch.object(runtime, 'batch_to_probs_and_masks', side_effect=predict), patch.object(runtime, 'report_worker_progress', side_effect=observe):
                    model.return_value.fixed_batch = None
                    runtime.run_worker(args)
                self.assertEqual(captured, expected)
                with rasterio.open(root/'mask.tif') as result:
                    self.assertTrue(np.all(result.read(1) == 255))


if __name__ == '__main__':
    unittest.main()
