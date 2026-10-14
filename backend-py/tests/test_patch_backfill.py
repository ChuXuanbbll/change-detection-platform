"""Historical repair must reproduce current vectorization without changing geometry."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

import numpy as np
import rasterio
from rasterio.transform import from_origin

from app.postprocess.vectorize import mask_to_geojson

spec = importlib.util.spec_from_file_location(
    "enrich_patch_areas", Path(__file__).resolve().parents[2] / "scripts/enrich_patch_areas.py")
repair = importlib.util.module_from_spec(spec)
spec.loader.exec_module(repair)


class BackfillTests(unittest.TestCase):
    def test_repair_holes_distinct_scores_and_preserve_original_backup(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            mask = np.zeros((12, 12), dtype="uint8")
            mask[1:6, 1:6] = 1
            mask[2:5, 2:5] = 0
            mask[8:10, 8:11] = 1
            confidence = np.where(mask, 0.9, 0.1).astype("float32")
            confidence[8:10, 8:11] = 0.7
            profile = dict(driver="GTiff", height=12, width=12, count=1,
                           crs="EPSG:4326", transform=from_origin(114.3, 38.1, 0.0001, 0.0001))
            for name, array in [("mask", mask), ("confidence", confidence)]:
                with rasterio.open(root / f"test_{name}.tif", "w", dtype=array.dtype, **profile) as dst:
                    dst.write(array, 1)
            path = root / "change.geojson"
            mask_to_geojson(str(root / "test_mask.tif"), str(root / "test_confidence.tif"), str(path))
            expected = json.loads(path.read_text())
            legacy = json.loads(path.read_text())
            for feature in legacy["features"]:
                feature["properties"] = {"area_pixels": 415, "confidence_mean": 0.5}
            original = json.dumps(legacy)
            path.write_text(original)
            audit = repair.inspect_result(path)
            self.assertEqual(path.read_text(), original)
            self.assertEqual(len(audit["score_mismatches"]), 2)
            repair.inspect_result(path, apply=True)
            actual = json.loads(path.read_text())
            for old, new in zip(expected["features"], actual["features"]):
                self.assertEqual(old["geometry"], new["geometry"])
                for field in ["area_m2", "area_pixels", "confidence_mean", "confidence_valid_pixels"]:
                    self.assertEqual(old["properties"][field], new["properties"][field])
            self.assertEqual(repair.inspect_result(path, apply=True)["score_mismatches"], [])
            self.assertEqual(path.with_suffix(".geojson.before-area-backfill.bak").read_text(), original)
