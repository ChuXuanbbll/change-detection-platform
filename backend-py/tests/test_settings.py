import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import numpy as np
import rasterio
from rasterio.transform import from_origin

from app import settings
from app.fetchers import local, imagery_index
from app.postprocess.pipeline import postprocess
from app.postprocess.vectorize import mask_to_geojson
from app.routers.admin import ModelProbe, validate_model
from app.utils import model_process


class SettingsTests(unittest.TestCase):
    def test_reject_overlapping_roots_and_invalid_parameters(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(ValueError):
                settings.Settings(
                    sources=[
                        settings.Source(id='a', name='a', path=tmp),
                        settings.Source(id='b', name='b', path=str(Path(tmp) / 'child')),
                    ]
                )
        for payload in (
            {'threshold': float('nan')},
            {'resolution': 0},
            {'kernel_size': 0},
            {'min_area_pixels': -1},
            {'unknown': 1},
        ):
            with self.subTest(payload=payload), self.assertRaises(ValueError):
                settings.Detection(**payload)

    def test_date_comes_from_directory_and_explicit_year(self):
        root = Path('2025年')
        self.assertEqual(
            imagery_index.capture_month(root / '1月' / '202606.tif', root, 2024)[0], '2024-01-01'
        )
        with self.assertRaises(ValueError):
            imagery_index.capture_month(root / '202501.tif', root)

    def test_multisource_identity_priority_and_unavailable_source(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            sources = []
            for index, value in enumerate((60, 180)):
                folder = root / str(index) / '保定市' / '1月'
                folder.mkdir(parents=True)
                path = folder / 'same.tif'
                with rasterio.open(
                    path,
                    'w',
                    driver='GTiff',
                    width=8,
                    height=8,
                    count=3,
                    dtype='uint8',
                    crs='EPSG:4326',
                    transform=from_origin(115, 39, 0.001, 0.001),
                ) as dst:
                    dst.write(np.full((3, 8, 8), value, dtype='uint8'))
                    valid = np.full((8, 8), 255, dtype='uint8')
                    valid[:, 0] = 0 if index == 0 else 255
                    dst.write_mask(valid)
                sources.append(
                    settings.Source(
                        id=f's{index}', name=str(index), path=str(root / str(index)), year=2024
                    )
                )
            with patch.object(settings, 'DATA_ROOT', root / 'cache'):
                rows = local.build_source_catalog(sources)
                self.assertEqual(len({r['id'] for r in rows}), 2)
                self.assertEqual(len(local.imagery_phases(rows, '130600')), 1)
                rgb, valid = local.read_mosaic(
                    rows, 'EPSG:4326', from_origin(115, 39, 0.001, 0.001), 8, 8
                )
                self.assertTrue(valid.all())
                self.assertTrue((rgb[:, :, 0] == 180).all())
                self.assertTrue((rgb[:, :, 1:] == 60).all())
                self.assertNotEqual(
                    local.mosaic_version(rows),
                    local.mosaic_version(
                        [{**r, 'source_priority': 1 - r['source_priority']} for r in rows]
                    ),
                )
                sources[1].enabled = False
                self.assertEqual(len(local.build_source_catalog(sources)), 1)
                sources[1].enabled = True
                sources[1].path = str(root / 'disconnected')
                with self.assertRaises(OSError):
                    local.build_source_catalog(sources)
        imagery_index.clear_cache()

    def test_four_neighbor_filter_eight_neighbor_statistics(self):
        mask = np.eye(3, dtype='uint8')
        conf = mask.astype('float32')
        cleaned = postprocess(
            mask, conf, {'threshold': 0.5, 'kernel_size': 1, 'min_area_pixels': 2}
        )
        self.assertFalse(cleaned.any())  # Diagonal-only pixels are separate for filtering.
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name, array in [('mask', mask), ('confidence', conf)]:
                with rasterio.open(
                    root / f'{name}.tif',
                    'w',
                    driver='GTiff',
                    width=3,
                    height=3,
                    count=1,
                    dtype=str(array.dtype),
                    crs='EPSG:4326',
                    transform=from_origin(115, 39, 0.001, 0.001),
                ) as dst:
                    dst.write(array, 1)
            result = mask_to_geojson(
                str(root / 'mask.tif'),
                str(root / 'confidence.tif'),
                str(root / 'result.json'),
                min_area_pixels=1,
            )
            self.assertEqual(result['feature_count'], 1)
            self.assertEqual(result['changed_area_pixels'], 3)
            reused = mask_to_geojson(
                str(root / 'mask.tif'),
                str(root / 'confidence.tif'),
                str(root / 'reused.json'),
                min_area_pixels=1,
                mask_array=mask,
                confidence_array=conf,
            )
            self.assertEqual(result, reused)
            self.assertEqual(
                (root / 'result.json').read_bytes(), (root / 'reused.json').read_bytes()
            )

    def test_model_mapping_and_cuda_failure_cannot_be_ready(self):
        import onnx
        from onnx import helper, TensorProto

        with tempfile.TemporaryDirectory() as tmp, patch.object(settings, 'DATA_ROOT', Path(tmp)):
            root = Path(tmp)
            path = root / 'dual.onnx'
            inputs = [
                helper.make_tensor_value_info(n, TensorProto.FLOAT, [1, 3, 512, 512])
                for n in ('before', 'after')
            ]
            output = helper.make_tensor_value_info('change', TensorProto.FLOAT, [1, 2, 512, 512])
            graph = helper.make_graph(
                [helper.make_node('Add', ['before', 'after'], ['change'])], 'test', inputs, [output]
            )
            onnx.save(helper.make_model(graph, opset_imports=[helper.make_opsetid('', 17)]), path)
            report = validate_model(ModelProbe(path=str(path)))
            self.assertEqual(report['status'], 'needs_mapping')
            with patch.object(model_process, 'run') as run:
                run.return_value = __import__('subprocess').CompletedProcess(
                    [], 1, '', 'CUDA unavailable'
                )
                report = validate_model(
                    ModelProbe(
                        path=str(path),
                        mapping={'input_a': 'before', 'input_b': 'after', 'output': 'change'},
                    )
                )
                self.assertEqual(report['status'], 'invalid')
                self.assertIn('CUDA', report['error'])
            with self.assertRaises(Exception):
                validate_model(ModelProbe(path=str(path), sha256='tampered'))

    def test_process_timeout_terminates_owned_child(self):
        import sys

        with patch.object(model_process, 'cuda_directory', return_value=''):
            with self.assertRaises(TimeoutError):
                model_process.run(
                    [sys.executable, '-c', 'import time; time.sleep(30)'], timeout=0.1
                )
        self.assertFalse(model_process._running)

    def test_incompatible_model_contracts_never_start_cuda(self):
        import onnx
        from onnx import helper, TensorProto
        from fastapi import HTTPException

        cases = [
            ([1, 4, 512, 512], [1, 2, 512, 512], {}),
            ([1, 6, 512, 512], [1, 3, 512, 512], {}),
            ([1, 6, 512, 512], [1, 2, 512, 512], {'input_a': 'missing'}),
        ]
        with tempfile.TemporaryDirectory() as tmp, patch.object(settings, 'DATA_ROOT', Path(tmp)):
            for index, (input_shape, output_shape, mapping) in enumerate(cases):
                with self.subTest(index=index):
                    path = Path(tmp) / f'{index}.onnx'
                    graph = helper.make_graph(
                        [helper.make_node('Identity', ['image'], ['change'])],
                        'contract',
                        [helper.make_tensor_value_info('image', TensorProto.FLOAT, input_shape)],
                        [helper.make_tensor_value_info('change', TensorProto.FLOAT, output_shape)],
                    )
                    onnx.save(helper.make_model(graph), path)
                    with patch.object(model_process, 'run') as run:
                        if index < 2:
                            with self.assertRaises(HTTPException):
                                validate_model(ModelProbe(path=str(path)))
                        else:
                            report = validate_model(ModelProbe(path=str(path), mapping=mapping))
                            self.assertEqual(report['status'], 'needs_mapping')
                        run.assert_not_called()


class ApplyTests(unittest.IsolatedAsyncioTestCase):
    async def test_defaults_reuse_renderer_and_failed_prepare_preserves_active(self):
        from app.routers import admin
        from types import SimpleNamespace

        class Request:
            app = SimpleNamespace(state=SimpleNamespace(tile_renderer=object()))

            async def is_disconnected(self):
                return False

        request = Request()
        old_renderer = request.app.state.tile_renderer
        old = settings.Settings(sources=[])
        with (
            patch.object(settings, 'active_settings', old),
            patch.object(settings, 'active_revision', 1),
            patch.object(local, 'catalog', return_value=[]),
        ):
            updated = old.model_copy(deep=True)
            updated.detection.threshold = 0.7
            with patch.object(admin, 'TileRenderer') as renderer:
                await admin.apply(admin.Apply(revision=2, settings=updated), request)
                renderer.assert_not_called()
                self.assertIs(request.app.state.tile_renderer, old_renderer)
                self.assertEqual(settings.active_revision, 2)
            changed = updated.model_copy(deep=True)
            changed.sources = [
                settings.Source(id='bad', name='bad', path=str(Path.cwd() / 'missing'))
            ]
            with patch.object(
                local, 'build_source_catalog', side_effect=OSError('shared drive disconnected')
            ):
                with self.assertRaises(Exception):
                    await admin.apply(admin.Apply(revision=3, settings=changed), request)
            self.assertEqual(settings.active_revision, 2)
            self.assertIs(request.app.state.tile_renderer, old_renderer)

    async def test_renderer_start_failure_preserves_active(self):
        from app.routers import admin
        from types import SimpleNamespace
        from unittest.mock import AsyncMock

        old = settings.Settings(sources=[])
        old_renderer = object()
        request = SimpleNamespace(
            app=SimpleNamespace(state=SimpleNamespace(tile_renderer=old_renderer))
        )
        updated = old.model_copy(deep=True)
        updated.tiles.concurrency = 2
        failed_renderer = SimpleNamespace(
            start=AsyncMock(side_effect=RuntimeError('worker start failed'))
        )
        with (
            patch.object(settings, 'active_settings', old),
            patch.object(settings, 'active_revision', 1),
            patch.object(local, 'catalog', return_value=[]),
            patch.object(admin, 'TileRenderer', return_value=failed_renderer),
        ):
            with self.assertRaises(RuntimeError):
                await admin.apply(admin.Apply(revision=2, settings=updated), request)
            self.assertEqual(settings.active_revision, 1)
            self.assertIs(settings.active_settings, old)
            self.assertIs(request.app.state.tile_renderer, old_renderer)


if __name__ == '__main__':
    unittest.main()
