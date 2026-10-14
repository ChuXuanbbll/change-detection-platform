"""变化检测主路由：调度算法 → 后处理 → 矢量化 → 统计。"""
from __future__ import annotations

import logging
import traceback
from pathlib import Path
from typing import Any, Dict

import numpy as np
import rasterio
import rasterio.warp
from rasterio.enums import Resampling
from rasterio.windows import from_bounds
from fastapi import APIRouter, HTTPException
from pydantic import BaseModel, Field

from app.algorithms import get
from app.postprocess.pipeline import postprocess
from app.postprocess.vectorize import mask_to_geojson

logger = logging.getLogger("app.routers.detect")

router = APIRouter(prefix="/py", tags=["detect"])


class DetectRequest(BaseModel):
    before_path: str = Field(..., description="前时相 GeoTIFF 路径")
    after_path: str = Field(..., description="后时相 GeoTIFF 路径")
    algorithm: str = Field("hebei_onnx", description="已注册的算法 name，默认 hebei_onnx")
    params: Dict[str, Any] = Field(default_factory=dict)
    output_dir: str = Field(..., description="产物输出目录（绝对路径）")


def _check_file(path: str) -> None:
    """文件不存在 → 400；其他错误交给调用方处理。"""
    if not path:
        raise HTTPException(
            status_code=400,
            detail={"error": "FILE_NOT_FOUND", "path": path or "",
                    "message": "path 为空"},
        )
    if not Path(path).exists():
        raise HTTPException(
            status_code=400,
            detail={"error": "FILE_NOT_FOUND", "path": path,
                    "message": f"输入文件不存在: {path}"},
        )


def _align_pair(before_path: str, after_path: str, tmp_dir: Path):
    """把 before/after 对齐到同一空间网格，返回 (aligned_before_path, aligned_after_path)。

    - 若两影像 CRS / transform / 尺寸完全一致：直接返回原路径（免额外 IO）。
    - 否则：计算两者的空间交集（相交区域），以 before 网格为基准，
      各自裁剪到交集并统一分辨率，重采样生成两张对齐后的临时 GeoTIFF。
    """
    bp, ap = Path(before_path), Path(after_path)
    with rasterio.Env(GDAL_CACHEMAX=128 * 1024 * 1024), rasterio.open(bp) as sb, rasterio.open(ap) as sa:
        if (
            sb.crs == sa.crs and sb.count == sa.count
            and sb.transform == sa.transform
            and sb.width == sa.width and sb.height == sa.height
        ):
            from rasterio.windows import Window
            for row in range(0, sb.height, 512):
                for col in range(0, sb.width, 512):
                    window = Window(col, row, min(512, sb.width-col), min(512, sb.height-row))
                    if ((sb.dataset_mask(window=window) > 0) & (sa.dataset_mask(window=window) > 0)).any():
                        return str(bp), str(ap)
            raise HTTPException(400, detail='前后影像没有共同有效像元')

        # Transform the second extent into the reference CRS before intersecting.
        from rasterio.vrt import WarpedVRT
        from rasterio.windows import Window
        after_bounds = rasterio.warp.transform_bounds(sa.crs, sb.crs, *sa.bounds)
        west = max(sb.bounds.left, after_bounds[0])
        south = max(sb.bounds.bottom, after_bounds[1])
        east = min(sb.bounds.right, after_bounds[2])
        north = min(sb.bounds.top, after_bounds[3])
        if west >= east or south >= north:
            raise HTTPException(400, detail={"error": "NO_OVERLAP", "message": "前后影像无空间交集"})
        raw = from_bounds(west, south, east, north, sb.transform)
        import math
        left, top = math.ceil(raw.col_off), math.ceil(raw.row_off)
        right, bottom = math.floor(raw.col_off + raw.width), math.floor(raw.row_off + raw.height)
        width, height = right-left, bottom-top
        if width <= 0 or height <= 0:
            raise HTTPException(400, detail="交集不足一个像元")
        win = Window(left, top, width, height)
        dst_transform = rasterio.windows.transform(win, sb.transform)
        bands = min(sb.count, sa.count)
        profile = sb.profile.copy()
        profile.update(driver="GTiff", width=width, height=height, transform=dst_transform,
                       count=bands, dtype=sb.dtypes[0], nodata=None, BIGTIFF='IF_SAFER')
        tmp_b = tmp_dir / "aligned_before.tif"
        tmp_a = tmp_dir / "aligned_after.tif"
        any_valid = False
        with rasterio.Env(GDAL_TIFF_INTERNAL_MASK=True), WarpedVRT(
                sa, crs=sb.crs, transform=dst_transform, width=width, height=height,
                resampling=Resampling.bilinear, add_alpha=True) as vrt, \
                rasterio.open(tmp_b, "w", **profile) as dst_b, \
                rasterio.open(tmp_a, "w", **profile) as dst_a:
            for row in range(0, height, 512):
                for col in range(0, width, 512):
                    target = Window(col, row, min(512, width-col), min(512, height-row))
                    source = Window(left+col, top+row, target.width, target.height)
                    b_arr = sb.read(list(range(1, bands+1)), window=source)
                    a_arr = vrt.read(list(range(1, bands+1)), window=target).astype(b_arr.dtype)
                    valid = (sb.dataset_mask(window=source) > 0) & (vrt.read(vrt.count, window=target) > 0)
                    any_valid |= bool(valid.any())
                    b_arr[:, ~valid] = 0
                    a_arr[:, ~valid] = 0
                    dst_b.write(b_arr, window=target)
                    dst_a.write(a_arr, window=target)
                    dst_b.write_mask(valid.astype('uint8') * 255, window=target)
                    dst_a.write_mask(valid.astype('uint8') * 255, window=target)
        if not any_valid:
            tmp_b.unlink(missing_ok=True)
            tmp_a.unlink(missing_ok=True)
            raise HTTPException(400, detail="前后影像没有共同有效像元")
    return str(tmp_b), str(tmp_a)


def _read_mask_confidence(mask_path: Path, confidence_path: Path):
    """从 GeoTIFF 读出 mask / confidence 数组（不做波段处理）。"""
    with rasterio.open(mask_path) as src:
        mask = src.read(1)
    with rasterio.open(confidence_path) as src:
        confidence = src.read(1)
    return mask.astype(np.uint8), confidence.astype(np.float32)


def _compute_stats(mask: np.ndarray, confidence: np.ndarray) -> Dict[str, Any]:
    changed = int(mask.sum())
    total = int(mask.size)
    finite_confidence = confidence[np.isfinite(confidence)]
    mean_conf_all = float(finite_confidence.mean()) if finite_confidence.size else 0.0
    if mask.any():
        vals = confidence[mask.astype(bool)]
        vals = vals[np.isfinite(vals)]
        mean_conf_changed = float(vals.mean()) if vals.size else 0.0
    else:
        mean_conf_changed = 0.0
    return {
        "changed_pixels": changed,
        "total_pixels": total,
        "change_ratio": (changed / total) if total else 0.0,
        "mean_confidence": round(mean_conf_changed, 6),
        "mean_confidence_all": round(mean_conf_all, 6),
        "confidence_histograms": _confidence_histograms(mask, confidence),
    }


def _confidence_histograms(mask: np.ndarray, confidence: np.ndarray) -> Dict[str, Any]:
    """Aggregate reporting-grid scores; caller already selects jointly valid pixels.

    Zero is a valid score. Ignore nonfinite/out-of-range values, and keep 1.0
    in the closed final bin. Counts describe model scores, not accuracy.
    """
    edges = np.linspace(0.0, 1.0, 21)
    finite = np.isfinite(confidence) & (confidence >= 0) & (confidence <= 1)
    all_scores = confidence[finite]
    changed_scores = confidence[finite & mask.astype(bool)]
    return {
        "bin_edges": edges.tolist(),
        "changed_counts": np.histogram(changed_scores, bins=edges)[0].tolist(),
        "all_valid_counts": np.histogram(all_scores, bins=edges)[0].tolist(),
        "changed_sample_count": int(changed_scores.size),
        "all_valid_sample_count": int(all_scores.size),
    }


@router.post("/detect")
def detect(req: DetectRequest) -> dict:
    """运行一次变化检测。

    流程：
      1. 校验 algorithm 与 before/after 文件
      2. 调 ``algo.detect(...)`` → 写 ``{algo}_mask.tif`` / ``{algo}_confidence.tif``
      3. 调 ``postprocess(...)`` 覆盖 mask.tif（形态学 + 最小图斑过滤）
      4. 调 ``mask_to_geojson(...)`` → ``change.geojson``
      5. 计算 ``changed_pixels / change_ratio / mean_confidence``
      6. 返回结果
    """
    # ---- 1) 校验 algorithm ----
    try:
        algo = get(req.algorithm)
    except KeyError:
        raise HTTPException(
            status_code=400,
            detail={"error": "ALGORITHM_NOT_FOUND",
                    "name": req.algorithm,
                    "message": f"算法未注册: {req.algorithm!r}"},
        )

    if req.algorithm == "hebei_onnx" and req.params.get("provider", "cuda") != "cuda":
        raise HTTPException(400, detail={"error": "GPU_REQUIRED",
                            "message": "hebei_onnx 模型检测必须使用 GPU（provider=cuda）"})

    from app.settings import Detection
    try:
        public = {k:v for k,v in req.params.items() if k not in ('provider', '_execution')}
        Detection.model_validate(public)
    except ValueError as exc:
        raise HTTPException(400, detail=str(exc)) from exc

    # ---- 2) 校验输入文件 ----
    _check_file(req.before_path)
    _check_file(req.after_path)

    output_dir = Path(req.output_dir).expanduser()
    if not output_dir.is_absolute():
        raise HTTPException(
            status_code=400,
            detail={"error": "OUTPUT_DIR_NOT_ABSOLUTE",
                    "path": str(output_dir),
                    "message": "output_dir 必须是绝对路径（共享卷约定 /data/...）"},
        )
    output_dir.mkdir(parents=True, exist_ok=True)

    from app.utils.progress import Progress
    progress = Progress(output_dir)
    progress("aligning", 0)

    # ---- 3) 对齐 before/after 到相交区域（避免影像尺寸/范围不一致导致广播失败） ----
    try:
        aligned_before, aligned_after = _align_pair(
            req.before_path, req.after_path, output_dir
        )
    except HTTPException:
        raise
    except Exception as exc:
        tb = traceback.format_exc()
        logger.exception("影像对齐失败")
        raise HTTPException(
            status_code=500,
            detail={
                "error": "ALIGN_FAILED",
                "message": f"{type(exc).__name__}: {exc}",
                "traceback": tb,
            },
        )

    # ---- 4) 算法主流程（写中间 mask.tif / confidence.tif） ----
    try:
        algo.detect(
            before_path=aligned_before,
            after_path=aligned_after,
            params=req.params,
            output_dir=output_dir,
        )
    except HTTPException:
        raise
    except Exception as exc:
        tb = traceback.format_exc()
        logger.exception("算法 %s 执行失败", req.algorithm)
        raise HTTPException(
            status_code=500,
            detail={
                "error": "ALGORITHM_EXEC_FAILED",
                "algorithm": req.algorithm,
                "message": f"{type(exc).__name__}: {exc}",
                "traceback": tb,
            },
        )

    # 算法内部已落盘：约定路径
    algo_name = getattr(algo, "name", req.algorithm)
    mask_path = output_dir / f"{algo_name}_mask.tif"
    confidence_path = output_dir / f"{algo_name}_confidence.tif"

    if not mask_path.exists() or not confidence_path.exists():
        raise HTTPException(
            status_code=500,
            detail={
                "error": "ALGORITHM_OUTPUT_MISSING",
                "algorithm": algo_name,
                "expected_mask": str(mask_path),
                "expected_confidence": str(confidence_path),
                "message": "算法未生成 mask.tif / confidence.tif",
            },
        )

    progress("postprocessing", 90)

    # ---- 5) 后处理（覆盖 mask.tif） ----
    try:
        mask_arr, conf_arr = _read_mask_confidence(mask_path, confidence_path)
        with rasterio.open(aligned_before) as before, rasterio.open(aligned_after) as after:
            valid = (before.dataset_mask() > 0) & (after.dataset_mask() > 0)
        if not valid.any():
            raise HTTPException(400, detail='前后影像没有共同有效像元')
        mask_arr[~valid] = 0
        conf_arr[~valid] = 0
        cleaned = postprocess(mask_arr, conf_arr, req.params, valid_mask=valid)
        cleaned[~valid] = 0
        with rasterio.Env(GDAL_TIFF_INTERNAL_MASK=True), rasterio.open(confidence_path, "r+") as confidence_dst:
            confidence_dst.write(conf_arr, 1)
            confidence_dst.write_mask(valid.astype('uint8') * 255)
        # 写回 mask.tif（profile 沿用原文件）
        with rasterio.open(mask_path, "r") as src:
            profile = src.profile.copy()
        with rasterio.Env(GDAL_TIFF_INTERNAL_MASK=True), rasterio.open(mask_path, "w", **profile) as dst:
            dst.write(cleaned.astype(np.uint8), 1)
            dst.write_mask(valid.astype('uint8') * 255)
    except HTTPException:
        raise
    except Exception as exc:
        tb = traceback.format_exc()
        logger.exception("postprocess 失败")
        raise HTTPException(
            status_code=500,
            detail={
                "error": "POSTPROCESS_FAILED",
                "message": f"{type(exc).__name__}: {exc}",
                "traceback": tb,
            },
        )

    # ---- 6) 矢量化 → GeoJSON ----
    progress("vectorizing", 94)
    geojson_path = output_dir / "change.geojson"
    try:
        vec_meta = mask_to_geojson(
            str(mask_path), str(confidence_path), str(geojson_path),
            min_area_pixels=int(req.params.get("min_area_pixels", 2)),
            mask_array=cleaned, confidence_array=conf_arr,
            progress=lambda done, total: progress("vectorizing", 94 + 4 * done / max(1, total)),
        )
    except Exception as exc:
        tb = traceback.format_exc()
        logger.exception("vectorize 失败")
        raise HTTPException(
            status_code=500,
            detail={
                "error": "VECTORIZE_FAILED",
                "message": f"{type(exc).__name__}: {exc}",
                "traceback": tb,
            },
        )

    # ---- 7) 统计 ----
    progress("statistics", 98)
    stats = _compute_stats(cleaned[valid], conf_arr[valid])
    stats["execution"] = req.params.get("_execution", {})
    stats["parameters"] = {k:v for k,v in req.params.items() if not k.startswith("_")}
    stats["algorithm"] = algo_name
    stats["feature_count"] = int(vec_meta.get("feature_count", 0))
    stats["changed_area_pixels"] = int(vec_meta.get("changed_area_pixels", 0))
    stats["changed_area_m2"] = float(vec_meta.get("changed_area_m2", 0))

    progress("saving", 99)
    return {
        "mask_path": str(mask_path),
        "confidence_path": str(confidence_path),
        "geojson_path": str(geojson_path),
        "stats": stats,
    }
