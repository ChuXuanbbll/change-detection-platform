"""Generate WGS84 change patches with component statistics and metric geometry."""
from __future__ import annotations

import json
from typing import Any, Dict, Callable

import numpy as np
import rasterio
from rasterio import features
from rasterio.warp import transform_geom
from scipy import ndimage
from shapely import make_valid
from shapely.geometry import mapping, shape


def mask_to_geojson(
    mask_path: str,
    confidence_path: str,
    geojson_path: str,
    min_area_pixels: int = 2,
    *, progress: Callable[[int, int], None] | None = None, mask_array: np.ndarray | None = None, confidence_array: np.ndarray | None = None,
) -> Dict[str, Any]:
    """Export one feature per 8-connected patch; scores use its changed pixels.

    Geometry, bbox and centroid use WGS84 longitude/latitude. Area uses the
    EPSG:6933 equal-area projection; perimeter uses the patch's local UTM zone.
    These describe the detected raster footprint, not surveyed ground truth.
    """
    with rasterio.open(mask_path) as src:
        mask = (src.read(1, masked=True).filled(0) if mask_array is None else mask_array) > 0
        grid_transform, grid_crs = src.transform, src.crs
    if grid_crs is None:
        raise ValueError("变化掩膜缺少 CRS，无法计算图斑地理位置和面积")

    with rasterio.open(confidence_path) as src:
        if src.shape != mask.shape or src.transform != grid_transform or src.crs != grid_crs:
            raise ValueError("置信度栅格与变化掩膜必须使用相同的尺寸、坐标系和像元网格")
        confidence = src.read(1, masked=True).astype(np.float32).filled(np.nan) if confidence_array is None else confidence_array
        if confidence.shape != mask.shape:
            raise ValueError("传入的统计数组网格不一致")

    # Polygonize the label raster, not the binary mask: each shape then carries
    # its actual component ID instead of the shared changed value 1.
    labels, count = ndimage.label(mask, structure=np.ones((3, 3), dtype=bool))
    sizes = np.bincount(labels.ravel(), minlength=count + 1)
    finite = mask & np.isfinite(confidence)
    score_counts = np.bincount(labels[finite], minlength=count + 1)
    score_sums = np.bincount(labels[finite], weights=confidence[finite], minlength=count + 1)

    features_out = []
    total_pixels = 0
    total_area = 0.0
    processed = 0
    for geometry, label in features.shapes(
        labels.astype(np.int32, copy=False), mask=mask, transform=grid_transform, connectivity=8,
    ):
        processed += 1
        label_id = int(label)
        area_pixels = int(sizes[label_id])
        if area_pixels < max(1, int(min_area_pixels)):
            if progress:
                progress(processed, count)
            continue
        # Diagonally touching cells can yield a self-touching Polygon. Preserve
        # the connected patch as a valid MultiPolygon when needed.
        source_polygon = make_valid(shape(geometry))
        wgs84_polygon = shape(transform_geom(grid_crs, "EPSG:4326", mapping(source_polygon)))
        centroid = wgs84_polygon.centroid
        zone = max(1, min(60, int((centroid.x + 180) // 6) + 1))
        metric_crs = f"EPSG:{(32600 if centroid.y >= 0 else 32700) + zone}"
        area_m2 = shape(transform_geom(grid_crs, "EPSG:6933", mapping(source_polygon))).area
        perimeter_m = shape(transform_geom(grid_crs, metric_crs, mapping(source_polygon))).length
        valid_pixels = int(score_counts[label_id])
        confidence_mean = float(score_sums[label_id] / valid_pixels) if valid_pixels else None
        patch_id = f"patch-{label_id}"
        features_out.append({
            "type": "Feature",
            "id": patch_id,
            "bbox": list(wgs84_polygon.bounds),
            "geometry": mapping(wgs84_polygon),
            "properties": {
                "patch_id": patch_id,
                "value": 1,
                "change_type": "unclassified_change",
                "area_pixels": area_pixels,
                "area_m2": round(area_m2, 3),
                "area_ha": round(area_m2 / 10000, 6),
                "perimeter_m": round(perimeter_m, 3),
                "perimeter_crs": metric_crs,
                "centroid": [centroid.x, centroid.y],
                "confidence_mean": round(confidence_mean, 6) if confidence_mean is not None else None,
                "confidence_valid_pixels": valid_pixels,
            },
        })
        total_pixels += area_pixels
        total_area += area_m2
        if progress:
            progress(processed, count)

    if progress:
        progress(count, count)
    collection = {
        "type": "FeatureCollection",
        "schema_version": 2,
        "crs": {"type": "name", "properties": {"name": "EPSG:4326"}},
        "source_crs": str(grid_crs),
        "area_method": "equal_area_epsg6933",
        "confidence_definition": "mean_change_score_over_patch_pixels",
        "features": features_out,
    }
    with open(geojson_path, "w", encoding="utf-8") as output:
        json.dump(collection, output, ensure_ascii=False, allow_nan=False)
    return {
        "feature_count": len(features_out),
        "changed_area_pixels": total_pixels,
        "changed_area_m2": round(total_area, 3),
    }
