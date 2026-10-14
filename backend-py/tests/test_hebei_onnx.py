"""Regression coverage for metric inference and platform-grid output alignment."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import numpy as np
import rasterio
from rasterio.transform import from_origin
from rasterio.warp import calculate_default_transform
from fastapi import HTTPException

from app.routers.detect import DetectRequest, detect
from app.algorithms.hebei_onnx import HebeiONNXDetector


class HebeiONNXPolicyTests(unittest.TestCase):
    def test_default_model(self):
        request = DetectRequest(before_path="before.tif", after_path="after.tif", output_dir="/result")
        self.assertEqual(request.algorithm, "hebei_onnx")

    def test_non_cuda_requests_rejected_before_file_io(self):
        for provider in ("cpu", "auto", "", None):
            with self.subTest(provider=provider), self.assertRaises(HTTPException) as raised:
                detect(DetectRequest(before_path="missing.tif", after_path="missing.tif",
                                     params={"provider": provider}, output_dir="/result"))
            self.assertEqual(raised.exception.status_code, 400)
            self.assertEqual(raised.exception.detail["error"], "GPU_REQUIRED")

    def test_adapter_rejects_cpu_without_launching_inference(self):
        with patch("app.algorithms.hebei_onnx.model_process.run") as run:
            with self.assertRaisesRegex(ValueError, "GPU"):
                HebeiONNXDetector().detect("missing.tif", "missing.tif", {"provider": "cpu"}, "/result")
            run.assert_not_called()


class HebeiONNXGridTests(unittest.TestCase):
    def run_grid_case(self, crs, transform, resolution):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            before, after = root / "before.tif", root / "after.tif"
            valid = np.ones((77, 83), dtype=np.uint8) * 255
            valid[:5, :] = 0
            profile = dict(driver="GTiff", height=77, width=83, count=3,
                           dtype="uint8", crs=crs, transform=transform)
            for path in (before, after):
                with rasterio.open(path, "w", **profile) as dst:
                    dst.write(np.full((3, 77, 83), 100, dtype=np.uint8))
                    dst.write_mask(valid)
            script, model = root / "pipeline.py", root / "model.onnx"
            script.touch()
            model.touch()

            def fake_inference(cmd, **kwargs):
                def arg(name):
                    return cmd[cmd.index(name) + 1]
                self.assertEqual(arg("--provider"), "cuda")
                self.assertEqual(arg("--device"), "cuda:0")
                target = rasterio.crs.CRS.from_string(arg("--dst_crs"))
                self.assertTrue(target.is_projected)
                self.assertEqual(target.linear_units_factor[1], 1.0)
                if crs == "EPSG:4526":
                    self.assertEqual(target.to_epsg(), 4526)
                with rasterio.open(before) as src:
                    grid, width, height = calculate_default_transform(
                        src.crs, target, src.width, src.height, *src.bounds,
                        resolution=float(arg("--resolution")),
                    )
                self.assertGreater(height, 1)
                self.assertNotEqual((height, width), valid.shape)
                for key, dtype, value in (("--out_prob", "uint16", 52428),
                                          ("--out_mask", "uint8", 255)):
                    with rasterio.open(arg(key), "w", driver="GTiff", count=1,
                                       width=width, height=height, crs=target,
                                       transform=grid, dtype=dtype) as dst:
                        dst.write(np.full((height, width), value, dtype=dtype), 1)
                return subprocess.CompletedProcess(cmd, 0, "", "")

            with patch.dict(os.environ, {"CHANGE_PIPELINE_SCRIPT": str(script),
                                         "CHANGE_MODEL_PATH": str(model),
                                         "CHANGE_ONNX_PROVIDER": "cpu",
                                         "CHANGE_ONNX_PYTHON": "test-python"}), patch(
                "app.algorithms.hebei_onnx.model_process.run", side_effect=fake_inference
            ):
                result = detect(DetectRequest(
                    before_path=str(before), after_path=str(after), algorithm="hebei_onnx",
                    params={"resolution": resolution, "kernel_size": 1,
                            "min_area_pixels": 1, "threshold": 0.5},
                    output_dir=str(root / "result"),
                ))
            for key in ("mask_path", "confidence_path"):
                with rasterio.open(result[key]) as src:
                    self.assertEqual(src.shape, valid.shape)
                    self.assertEqual(src.transform, transform)
                    self.assertEqual(src.crs, rasterio.crs.CRS.from_string(crs))
                    values = src.read(1)
                    self.assertTrue(np.all(values[valid == 0] == 0))
                    self.assertTrue(np.all(values[10:-10, 10:-10] > 0))
                    if key == "confidence_path":
                        np.testing.assert_allclose(values[10:-10, 10:-10], 0.8, atol=1e-5)
            self.assertEqual(result["stats"]["total_pixels"], int((valid > 0).sum()))
            self.assertGreater(result["stats"]["changed_pixels"], 0)
            self.assertTrue(Path(result["geojson_path"]).exists())

    def test_geographic_input_77_rows(self):
        self.run_grid_case("EPSG:4326", from_origin(114.3, 38.1, 0.00001, 0.00001), 0.8)

    def test_projected_input_with_different_resolution(self):
        self.run_grid_case("EPSG:4526", from_origin(38500000, 4200000, 0.8, 0.8), 1.6)


if __name__ == "__main__":
    unittest.main()
