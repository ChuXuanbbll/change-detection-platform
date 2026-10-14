"""带地理参考的透明 PNG 叠加路由。

Python 把 ``mask.tif`` 渲染为一张透明 RGBA PNG（变化像素=指定颜色，
其余透明），并带上地理 bounds，供前端 Leaflet 用 ``L.imageOverlay`` 叠加
到底图上（前端无需 geotiff.js）。

提供两种返回方式：

* ``POST /py/overlay/mask`` —— JSON body，返回 base64 PNG + bounds 元数据；
* ``GET /py/overlay/mask.png`` —— 查询参数，直接返回 ``image/png`` 字节流，
  可被 ``<img>`` 标签直接引用（同源情况下）。
"""
from __future__ import annotations

import base64
import logging
import math
import traceback
from pathlib import Path

import numpy as np
import rasterio
from fastapi import APIRouter, HTTPException, Response
from pydantic import BaseModel, Field

logger = logging.getLogger("app.routers.overlay")

router = APIRouter(prefix="/py/overlay", tags=["overlay"])

# 下采样后最长边上限（避免传输超大 PNG）
MAX_EDGE = 512


class OverlayReq(BaseModel):
    mask_path: str = Field(..., description="变化掩膜 GeoTIFF 绝对路径")
    alpha: float = Field(0.45, ge=0.0, le=1.0, description="变化像素不透明度 0~1")
    color: str = Field("#ff0000", description="变化像素颜色，支持 #rrggbb 或命名色")


def _render_mask_rgba(
    src_path: Path, color: tuple[int, int, int], alpha: float
) -> tuple[np.ndarray, list[float], str]:
    """读 mask band1 → 下采样到 <=MAX_EDGE → 生成 RGBA (H,W,4) uint8。

    返回 (rgba 数组, bounds:[w,s,e,n], crs)。
    """
    with rasterio.open(src_path) as src:
        if src.count < 1:
            raise HTTPException(
                status_code=400,
                detail={
                    "error": "EMPTY_RASTER",
                    "message": "mask 栅格没有波段",
                },
            )
        bounds = src.bounds  # BoundingBox(left, bottom, right, top)
        crs = src.crs.to_string() if src.crs is not None else ""

        # 等比下采样，最长边 <= MAX_EDGE（bilinear 平滑掩膜边缘）
        scale = min(MAX_EDGE / float(src.height), MAX_EDGE / float(src.width), 1.0)
        if scale >= 1.0:
            arr = src.read(1)
        else:
            out_h = max(1, int(math.floor(src.height * scale)))
            out_w = max(1, int(math.floor(src.width * scale)))
            arr = src.read(
                1,
                out_shape=(out_h, out_w),
                resampling=rasterio.enums.Resampling.bilinear,
            )

        valid = src.dataset_mask(out_shape=arr.shape, resampling=rasterio.enums.Resampling.nearest) > 0

    # 二值化到 0/1：>0 视为变化（掩膜常见 0/1 或 0/255，nodeValue 通常为 1）
    mask = arr.astype(np.float32)
    mask = np.isfinite(mask) & (mask > 0) & valid

    h, w = mask.shape
    r, g, b = color
    rgba = np.zeros((h, w, 4), dtype=np.uint8)
    a = max(0.0, min(1.0, float(alpha))) * 255
    rgba[mask, 0] = r
    rgba[mask, 1] = g
    rgba[mask, 2] = b
    rgba[mask, 3] = int(round(a))

    return rgba, [bounds.left, bounds.bottom, bounds.right, bounds.top], crs


def _parse_color(color: str) -> tuple[int, int, int]:
    """解析颜色字符串为 (r,g,b)。失败抛 ValueError。"""
    from PIL import ImageColor

    try:
        rgb = ImageColor.getcolor(color, "RGB")
    except Exception:
        raise ValueError(f"无法解析颜色: {color!r}")
    return rgb


def _render_png_bytes(rgba: np.ndarray) -> bytes:
    """RGBA (H,W,4) uint8 → PNG 字节。"""
    from PIL import Image
    import io

    img = Image.fromarray(rgba, mode="RGBA")
    bio = io.BytesIO()
    img.save(bio, format="PNG", optimize=False)
    return bio.getvalue()


@router.post("/mask")
def overlay_mask(req: OverlayReq) -> dict:
    """返回 base64 PNG + bounds + 尺寸 + CRS，供前端 imageOverlay 叠加。"""
    src_p = Path(req.mask_path).expanduser()
    if not src_p.exists():
        raise HTTPException(
            status_code=400,
            detail={
                "error": "MASK_NOT_FOUND",
                "path": str(src_p),
                "message": "mask GeoTIFF 不存在",
            },
        )

    try:
        color = _parse_color(req.color)
    except ValueError as exc:
        raise HTTPException(
            status_code=400,
            detail={
                "error": "BAD_COLOR",
                "color": req.color,
                "message": str(exc),
            },
        )

    try:
        rgba, bounds, crs = _render_mask_rgba(src_p, color, req.alpha)
        png = _render_png_bytes(rgba)
    except HTTPException:
        raise
    except Exception as exc:
        tb = traceback.format_exc()
        logger.exception("overlay mask 渲染失败")
        raise HTTPException(
            status_code=500,
            detail={
                "error": "OVERLAY_FAILED",
                "mask_path": str(src_p),
                "message": f"{type(exc).__name__}: {exc}",
                "traceback": tb,
            },
        )

    return {
        "png_b64": base64.b64encode(png).decode("ascii"),
        "bounds": bounds,
        "width": int(rgba.shape[1]),
        "height": int(rgba.shape[0]),
        "crs": crs,
        "alpha": req.alpha,
        "color": req.color,
    }


@router.get("/mask.png")
def overlay_mask_png(
    path: str,
    alpha: float = 0.45,
    color: str = "#ff0000",
) -> Response:
    """直接返回原始 PNG 字节流（image/png），供 <img> 引用。"""
    src_p = Path(path).expanduser()
    if not src_p.exists():
        raise HTTPException(
            status_code=400,
            detail={
                "error": "MASK_NOT_FOUND",
                "path": str(src_p),
                "message": "mask GeoTIFF 不存在",
            },
        )

    try:
        rgb = _parse_color(color)
    except ValueError as exc:
        raise HTTPException(
            status_code=400,
            detail={"error": "BAD_COLOR", "color": color, "message": str(exc)},
        )

    try:
        rgba, _bounds, _crs = _render_mask_rgba(src_p, rgb, alpha)
        png = _render_png_bytes(rgba)
    except HTTPException:
        raise
    except Exception as exc:
        tb = traceback.format_exc()
        logger.exception("overlay mask.png 渲染失败")
        raise HTTPException(
            status_code=500,
            detail={
                "error": "OVERLAY_FAILED",
                "mask_path": str(src_p),
                "message": f"{type(exc).__name__}: {exc}",
                "traceback": tb,
            },
        )

    return Response(content=png, media_type="image/png")


# ---------------------------------------------------------------------------
# 影像渲染（RGB GeoTIFF → PNG），供中央地图直接叠加显示 before/after 影像
# ---------------------------------------------------------------------------
def _render_rgb_rgba(src_path: Path, max_edge: int = MAX_EDGE) -> tuple[np.ndarray, list[float], str]:
    """读 RGB GeoTIFF → 下采样到最长边 <= max_edge → RGBA (H,W,4) uint8（不透明）。

    返回 (rgba, bounds:[w,s,e,n], crs)。波段不足 3 时用第一个波段补齐灰度。
    """
    with rasterio.open(src_path) as src:
        bounds = src.bounds  # BoundingBox(left, bottom, right, top)
        crs = src.crs.to_string() if src.crs is not None else ""
        n = src.count

        scale = min(max_edge / float(src.height), max_edge / float(src.width), 1.0)
        if scale >= 1.0:
            arr = src.read([1, min(2, n), min(3, n)])
        else:
            out_h = max(1, int(math.floor(src.height * scale)))
            out_w = max(1, int(math.floor(src.width * scale)))
            arr = src.read(
                [1, min(2, n), min(3, n)],
                out_shape=(3, out_h, out_w),
                resampling=rasterio.enums.Resampling.bilinear,
            )
        alpha = src.dataset_mask(out_shape=arr.shape[1:], resampling=rasterio.enums.Resampling.nearest)
        arr = arr.astype(np.float32)
        # 归一化到 0-255（基于有效统计，避免整体偏暗/偏亮）
        valid = np.isfinite(arr) & (alpha[None, :, :] > 0)
        if valid.any():
            lo, hi = np.percentile(arr[valid], 2), np.percentile(arr[valid], 98)
        else:
            lo, hi = 0.0, 1.0
        scale_range = max(float(hi) - float(lo), 1e-6)
        norm = np.clip((arr - float(lo)) / scale_range, 0.0, 1.0) * 255.0

    h, w = norm.shape[1], norm.shape[2]
    rgb = np.zeros((h, w, 3), dtype=np.uint8)
    for i in range(3):
        rgb[:, :, i] = norm[i].astype(np.uint8)

    rgba = np.dstack([rgb, alpha])
    return rgba, [bounds.left, bounds.bottom, bounds.right, bounds.top], crs


class ImageOverlayReq(BaseModel):
    image_path: str = Field(..., description="RGB GeoTIFF 绝对路径")
    max_edge: int = Field(MAX_EDGE, ge=64, le=2048, description="渲染最长边上限")


@router.post("/image")
def overlay_image(req: ImageOverlayReq) -> dict:
    """读 RGB GeoTIFF 渲染为 PNG，返回 base64+地理 bounds，供前端叠加显示影像。"""
    src_p = Path(req.image_path).expanduser()
    if not src_p.exists():
        raise HTTPException(
            status_code=400,
            detail={
                "error": "IMAGE_NOT_FOUND",
                "path": str(src_p),
                "message": "影像 GeoTIFF 不存在",
            },
        )
    try:
        rgba, bounds, crs = _render_rgb_rgba(src_p, req.max_edge)
        png = _render_png_bytes(rgba)
    except HTTPException:
        raise
    except Exception as exc:
        tb = traceback.format_exc()
        logger.exception("overlay image 渲染失败")
        raise HTTPException(
            status_code=500,
            detail={
                "error": "OVERLAY_FAILED",
                "image_path": str(src_p),
                "message": f"{type(exc).__name__}: {exc}",
                "traceback": tb,
            },
        )

    return {
        "image_b64": base64.b64encode(png).decode("ascii"),
        "bounds": bounds,
        "width": int(rgba.shape[1]),
        "height": int(rgba.shape[0]),
        "crs": crs,
    }
