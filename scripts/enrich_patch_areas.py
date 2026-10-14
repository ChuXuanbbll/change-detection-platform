"""Backfill saved patch areas and repair per-patch counts and scores.

Run with backend-py's Python environment. Defaults to a dry run; --apply
backs up each original GeoJSON before atomically replacing it. Geometry is
preserved. Areas use the same EPSG:6933 method as vectorize.py.
"""
import argparse
import json
from pathlib import Path
import shutil

import numpy as np
import rasterio
from rasterio.features import geometry_mask, geometry_window
from rasterio.warp import transform_geom
from shapely import make_valid
from shapely.geometry import mapping, shape


def inspect_result(path, apply=False):
    collection = json.loads(path.read_text(encoding="utf-8"))
    crs = collection.get("crs", {}).get("properties", {}).get("name", "EPSG:4326")
    report = {"path": str(path), "patches": len(collection["features"]),
              "audited_scores": 0, "score_mismatches": [], "pixel_mismatches": 0, "matching_example": []}
    confidence_paths = list(path.parent.glob("*_confidence.tif"))
    if len(confidence_paths) != 1:
        raise ValueError(f"Expected one confidence raster: {path.parent}")
    confidence_path = confidence_paths[0]
    mask_path = confidence_path.with_name(confidence_path.name.replace("_confidence.tif", "_mask.tif"))
    with rasterio.open(confidence_path) as confidence, rasterio.open(mask_path) as mask:
        if (confidence.shape, confidence.transform, confidence.crs) != (mask.shape, mask.transform, mask.crs):
            raise ValueError(f"Mask/confidence grids differ: {path}")
        for index, feature in enumerate(collection["features"]):
            geometry = mapping(make_valid(shape(feature["geometry"])))
            area = shape(transform_geom(crs, "EPSG:6933", geometry)).area
            properties = feature["properties"]
            properties["area_m2"] = round(area, 3)
            properties["area_ha"] = round(area / 10000, 6)
            raster_geometry = transform_geom(crs, confidence.crs, geometry)
            window = geometry_window(confidence, [raster_geometry])
            scores = confidence.read(1, window=window, masked=True).astype("float64").filled(np.nan)
            changed = mask.read(1, window=window, masked=True).filled(0) > 0
            inside = geometry_mask([raster_geometry], out_shape=scores.shape,
                                   transform=confidence.window_transform(window), invert=True)
            valid = inside & changed & np.isfinite(scores)
            pixels = int((inside & changed).sum())
            if properties.get("area_pixels") != pixels:
                report["pixel_mismatches"] += 1
            mean = float(scores[valid].mean()) if valid.any() else None
            saved = properties.get("confidence_mean")
            matches = (saved is None and mean is None) or (
                saved is not None and mean is not None and abs(saved - mean) <= 1e-6)
            report["audited_scores"] += 1
            if not matches:
                report["score_mismatches"].append({"index": index, "saved": saved, "recomputed": mean})
            if properties.get("area_pixels") == 415 and saved is not None and abs(saved - 0.888827) < 1e-6:
                report["matching_example"].append({"index": index, "area_m2": properties["area_m2"],
                    "saved_score": saved, "recomputed_score": mean, "valid_pixels": int(valid.sum())})
            properties["area_pixels"] = pixels
            properties["confidence_mean"] = round(mean, 6) if mean is not None else None
            properties["confidence_valid_pixels"] = int(valid.sum())
    collection["area_method"] = "equal_area_epsg6933"
    collection["confidence_definition"] = "mean_change_score_over_patch_pixels"
    if apply:
        backup = path.with_suffix(".geojson.before-area-backfill.bak")
        if not backup.exists():
            shutil.copy2(path, backup)
        temporary = path.with_suffix(".geojson.area-tmp")
        temporary.write_text(json.dumps(collection, ensure_ascii=False, allow_nan=False), encoding="utf-8")
        temporary.replace(path)
    return report


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("results", type=Path)
    parser.add_argument("--apply", action="store_true")
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    reports = [inspect_result(path, args.apply) for path in sorted(args.results.glob("*/change.geojson"))]
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(json.dumps(reports, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps({"results": len(reports), "patches": sum(r["patches"] for r in reports),
                      "score_mismatches": sum(len(r["score_mismatches"]) for r in reports),
                      "applied": args.apply}))
