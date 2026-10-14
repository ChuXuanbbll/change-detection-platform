"""Patch statistics must agree with raster support, including holes and CRS units."""
import json
from pathlib import Path
import tempfile
import unittest

import numpy as np
import rasterio
from rasterio.transform import from_origin
from shapely.geometry import shape

from app.postprocess.vectorize import mask_to_geojson


class ChangePatchTests(unittest.TestCase):
    def export(self, mask, confidence, minimum=1, crs="EPSG:32650", transform=None,
               confidence_transform=None, confidence_nodata=None, progress=None):
        transform = transform or from_origin(500000, 4200000, 1, 1)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            profile = dict(driver="GTiff", height=mask.shape[0], width=mask.shape[1],
                           count=1, crs=crs, transform=transform)
            with rasterio.open(root / "mask.tif", "w", dtype="uint8", **profile) as dst:
                dst.write(mask, 1)
            profile["transform"] = confidence_transform or transform
            with rasterio.open(root / "confidence.tif", "w", dtype="float32",
                               nodata=confidence_nodata, **profile) as dst:
                dst.write(confidence.astype(np.float32), 1)
            stats = mask_to_geojson(str(root / "mask.tif"), str(root / "confidence.tif"),
                                    str(root / "change.geojson"), minimum, progress=progress)
            return json.loads((root / "change.geojson").read_text(encoding="utf-8")), stats

    def test_progress_counts_filtered_components_and_empty_masks(self):
        events = []
        collection, _ = self.export(*self.two_patches(), minimum=5, progress=lambda done,total: events.append((done,total)))
        self.assertEqual(events[-1], (2,2))
        self.assertIn((1,2), events)
        self.assertEqual(len(collection['features']),1)
        events.clear()
        self.export(np.zeros((4,4),dtype='uint8'),np.zeros((4,4),dtype='float32'), progress=lambda done,total: events.append((done,total)))
        self.assertEqual(events[-1], (0,0))

    def two_patches(self):
        mask = np.zeros((9, 10), dtype=np.uint8)
        mask[1:3, 1:3] = 1
        mask[5:7, 5:8] = 1
        confidence = np.zeros(mask.shape, dtype=np.float32)
        confidence[1:3, 1:3] = 0.9
        confidence[5:7, 5:8] = 0.7
        return mask, confidence

    def test_multiple_patches_keep_their_own_area_score_and_identity(self):
        collection, stats = self.export(*self.two_patches())
        patches = sorted(collection["features"], key=lambda f: f["properties"]["area_pixels"])
        self.assertEqual([f["properties"]["area_pixels"] for f in patches], [4, 6])
        self.assertEqual([f["properties"]["confidence_mean"] for f in patches], [0.9, 0.7])
        self.assertEqual(len({f["id"] for f in patches}), 2)
        self.assertEqual(stats["changed_area_pixels"], 10)
        self.assertEqual(stats["feature_count"], 2)
        self.assertEqual(collection["schema_version"], 2)
        self.assertEqual(collection["crs"]["properties"]["name"], "EPSG:4326")
        for feature in patches:
            properties = feature["properties"]
            self.assertAlmostEqual(properties["area_m2"], properties["area_pixels"], delta=0.02)
            self.assertAlmostEqual(properties["area_ha"], properties["area_m2"] / 10000, delta=1e-6)
            self.assertGreater(properties["perimeter_m"], 0)
            self.assertEqual(properties["value"], 1)
            self.assertEqual(properties["patch_id"], feature["id"])
            self.assertTrue(shape(feature["geometry"]).is_valid)
            self.assertTrue(115 < properties["centroid"][0] < 119)
            self.assertTrue(35 < properties["centroid"][1] < 40)
            self.assertEqual(list(shape(feature["geometry"]).bounds), feature["bbox"])

    def test_minimum_area_filters_each_component_independently(self):
        collection, stats = self.export(*self.two_patches(), minimum=5)
        self.assertEqual(len(collection["features"]), 1)
        self.assertEqual(collection["features"][0]["properties"]["area_pixels"], 6)
        self.assertEqual(stats["changed_area_pixels"], 6)

    def test_holes_do_not_dilute_patch_score(self):
        mask = np.zeros((9, 9), dtype=np.uint8)
        mask[2:7, 2:7] = 1
        mask[3:6, 3:6] = 0
        collection, _ = self.export(mask, np.where(mask, 0.9, 0))
        feature = collection["features"][0]
        self.assertEqual(feature["properties"]["area_pixels"], 16)
        self.assertEqual(feature["properties"]["confidence_mean"], 0.9)
        self.assertEqual(len(feature["geometry"]["coordinates"]), 2)

    def test_diagonal_cells_are_one_valid_patch(self):
        mask = np.zeros((4, 4), dtype=np.uint8)
        mask[1, 1] = mask[2, 2] = 1
        collection, stats = self.export(mask, np.where(mask, 0.8, 0))
        self.assertEqual(stats["feature_count"], 1)
        feature = collection["features"][0]
        self.assertEqual(feature["geometry"]["type"], "MultiPolygon")
        self.assertTrue(shape(feature["geometry"]).is_valid)
        self.assertEqual(feature["properties"]["area_pixels"], 2)

    def test_no_score_is_null_but_a_real_zero_stays_zero(self):
        mask, confidence = self.two_patches()
        confidence[1:3, 1:3] = np.nan
        confidence[5:7, 5:8] = 0
        collection, _ = self.export(mask, confidence)
        patches = sorted(collection["features"], key=lambda f: f["properties"]["area_pixels"])
        self.assertIsNone(patches[0]["properties"]["confidence_mean"])
        self.assertEqual(patches[0]["properties"]["confidence_valid_pixels"], 0)
        self.assertEqual(patches[1]["properties"]["confidence_mean"], 0)

    def test_confidence_nodata_is_excluded(self):
        mask, confidence = self.two_patches()
        confidence[1, 1] = -9999
        collection, _ = self.export(mask, confidence, confidence_nodata=-9999)
        first = next(f for f in collection["features"] if f["properties"]["area_pixels"] == 4)
        self.assertEqual(first["properties"]["confidence_valid_pixels"], 3)
        self.assertEqual(first["properties"]["confidence_mean"], 0.9)

    def test_geographic_area_is_in_square_metres(self):
        mask, confidence = self.two_patches()
        transform = from_origin(114.3, 38.1, 0.0001, 0.0001)
        collection, _ = self.export(mask, confidence, crs="EPSG:4326", transform=transform)
        for feature in collection["features"]:
            self.assertGreater(feature["properties"]["area_m2"], 100)
            self.assertLess(feature["properties"]["area_m2"], 1000)
            self.assertEqual(feature["bbox"], list(shape(feature["geometry"]).bounds))

    def test_misaligned_scores_are_rejected(self):
        with self.assertRaisesRegex(ValueError, "网格"):
            self.export(*self.two_patches(), confidence_transform=from_origin(500001, 4200000, 1, 1))

    def test_missing_crs_is_rejected_instead_of_inventing_area_units(self):
        with self.assertRaisesRegex(ValueError, "CRS"):
            self.export(*self.two_patches(), crs=None)

    def test_no_change_returns_an_empty_collection(self):
        mask = np.zeros((4, 4), dtype=np.uint8)
        collection, stats = self.export(mask, mask.astype(np.float32))
        self.assertEqual(collection["features"], [])
        self.assertEqual(stats, {"feature_count": 0, "changed_area_pixels": 0, "changed_area_m2": 0})
