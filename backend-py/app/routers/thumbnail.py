"""缩略图生成路由。

POST ``/py/thumbnail`` —— 读取 ``src_path`` 处的 GeoTIFF，做等比缩放后
写入 ``dst_path``（PNG）。

支持两种入参方式：

* 查询字符串 ``?path=&dst=&size=``（Go 端 PyClient 当前调用方式）
* JSON body ``{"src_path": "...", "dst_path": "...", "size": 256}``
"""
from __future__ import annotations

import logging
import math
import os
import shutil
import tempfile
import traceback
from pathlib import Path
from typing import Optional

import numpy as np
import rasterio
from fastapi import APIRouter, HTTPException
from pydantic import BaseModel, Field

logger = logging.getLogger("app.routers.thumbnail")

router = APIRouter(prefix="/py", tags=["thumbnail"])


class ThumbnailReq(BaseModel):
    src_path: str = Field(..., description="源 GeoTIFF 绝对路径")
    dst_path: str = Field(..., description="目标 PNG 绝对路径")
    size: int = Field(256, ge=16, le=4096, description="缩略图最长边（px）")


def _to_uint8_rgb(arr: np.ndarray) -> np.ndarray:
    """把任意 (bands, H, W) / (H, W) 数组转成 (H, W, 3) uint8 RGB。

    - 单波段 → 复制为 3 通道；
    - 多波段：取前 3 波段；
    - 非 uint8：按 nodata / dtype 简单 min-max 拉伸到 0-255。
    """
    if arr.ndim == 2:
        arr = arr[np.newaxis, :, :]

    bands = arr.shape[0]
    take = min(3, bands)
    rgb = arr[:take]

    if rgb.dtype != np.uint8:
        # 简单 min-max 拉伸；忽略 nan
        a = rgb.astype(np.float32)
        finite = np.isfinite(a)
        if finite.any():
            lo = float(a[finite].min())
            hi = float(a[finite].max())
        else:
            lo, hi = 0.0, 1.0
        if hi - lo < 1e-12:
            hi = lo + 1.0
        a = (a - lo) / (hi - lo) * 255.0
        a = np.clip(a, 0, 255)
        rgb = a.astype(np.uint8)

    # (bands, H, W) → (H, W, bands)
    rgb = np.transpose(rgb, (1, 2, 0))

    if rgb.shape[2] == 1:
        rgb = np.repeat(rgb, 3, axis=2)
    elif rgb.shape[2] == 2:
        rgb = np.concatenate([rgb, rgb[:, :, :1]], axis=2)
    return np.ascontiguousarray(rgb)


def _render(src_path: Path, dst_path: Path, size: int) -> int:
    """读 GeoTIFF → 缩放 → 写 PNG。返回写入字节数。"""
    # 懒导入 PIL，避免冷启动影响
    from PIL import Image

    with rasterio.open(src_path) as src:
        # 对大数据集：仅读取前 3 波段，并按 size 估算下采样
        bands = min(3, src.count)
        scale = min(size / float(src.height), size / float(src.width), 1.0)
        if scale >= 1.0:
            arr = src.read(list(range(1, bands + 1)))
        else:
            out_h = max(1, int(math.floor(src.height * scale)))
            out_w = max(1, int(math.floor(src.width * scale)))
            arr = src.read(
                list(range(1, bands + 1)),
                out_shape=(bands, out_h, out_w),
                resampling=rasterio.enums.Resampling.bilinear,
            )

        alpha = src.dataset_mask(out_shape=arr.shape[1:], resampling=rasterio.enums.Resampling.nearest)

    rgb = _to_uint8_rgb(arr)
    img = Image.fromarray(np.dstack((rgb, alpha)), mode="RGBA")

    dst_path.parent.mkdir(parents=True, exist_ok=True)
    # 用临时文件 + 原子替换，避免半截文件被外部读到
    tmp_fd, tmp_raw = tempfile.mkstemp(
        prefix=".thumb_", suffix=".png", dir=str(dst_path.parent)
    )
    os.close(tmp_fd)
    tmp_p = Path(tmp_raw)
    try:
        img.save(tmp_p, format="PNG", optimize=False)
        shutil.move(str(tmp_p), str(dst_path))
    finally:
        if tmp_p.exists():
            tmp_p.unlink()
    return dst_path.stat().st_size


@router.post("/thumbnail")
def make_thumbnail(
    src_path: Optional[str] = None,
    dst_path: Optional[str] = None,
    size: int = 256,
    body: Optional[ThumbnailReq] = None,
) -> dict:
    """生成缩略图。

    任一错误（路径缺失 / 打开失败 / IO 失败）→ HTTPException(400 / 500)
    + 结构化 detail。
    """
    # 兼容 query / JSON body
    if body is not None:
        src = body.src_path
        dst = body.dst_path
        sz = body.size
    else:
        src = src_path
        dst = dst_path
        sz = size

    if not src or not dst:
        raise HTTPException(
            status_code=400,
            detail={
                "error": "MISSING_PARAM",
                "message": "必须提供 src_path 与 dst_path（query 或 JSON body）",
            },
        )

    src_p = Path(src).expanduser()
    dst_p = Path(dst).expanduser()
    if not src_p.exists():
        raise HTTPException(
            status_code=400,
            detail={
                "error": "FILE_NOT_FOUND",
                "path": str(src_p),
                "message": "源 GeoTIFF 不存在",
            },
        )

    try:
        n = _render(src_p, dst_p, sz)
    except HTTPException:
        raise
    except Exception as exc:
        tb = traceback.format_exc()
        logger.exception("thumbnail 生成失败")
        raise HTTPException(
            status_code=500,
            detail={
                "error": "THUMBNAIL_FAILED",
                "src_path": str(src_p),
                "dst_path": str(dst_p),
                "message": f"{type(exc).__name__}: {exc}",
                "traceback": tb,
            },
        )

    return {
        "thumbnail_path": str(dst_p),
        "bytes": int(n),
        "src_path": str(src_p),
        "size": int(sz),
    }
