"""后处理管线：Otsu 阈值 → 形态学 → 最小图斑过滤。

主入口 :func:`postprocess` 严格按任务约定签名：
``postprocess(mask, confidence, params) -> mask``。

实现要点：
1. **Otsu 阈值** — ``params['threshold']`` 为 ``"auto"`` / ``None`` 时用 skimage 的 Otsu；
   显式 number 则直接用。原 mask 与高置信区域求并集。
2. **形态学** — 默认先开后闭去噪，核大小由 ``params['kernel_size']`` 控制。
3. **最小图斑** — ``skimage.morphology.remove_small_objects`` 按
   ``params['min_area_pixels']`` 过滤。

辅助函数 :func:`postprocess_pair` 兼容旧代码（同时返回 (mask, confidence)）。
"""
from __future__ import annotations

from typing import Any, Dict, Optional, Tuple

import numpy as np
from scipy import ndimage
from skimage.filters import threshold_otsu
from skimage.morphology import remove_small_objects


def _resolve_threshold(threshold, confidence: np.ndarray, mask: np.ndarray) -> float:
    """根据 threshold 参数解析出实际阈值；失败 fallback 到 0.5。"""
    # auto / None → 用 Otsu 在 confidence 上求
    if threshold is None or (isinstance(threshold, str) and threshold.lower() == "auto"):
        conf = confidence.astype(np.float32)
        if conf.size and float(conf.max()) > float(conf.min()):
            try:
                return float(threshold_otsu(conf))
            except Exception:
                return 0.5
        return 0.5

    try:
        t = float(threshold)
        # 显式 threshold 时把它当作 confidence 的下限；
        # 若 threshold > 1，假定它是 confidence 的绝对尺度（max≈1），
        # 此时不再二次归一化。
        return t
    except (TypeError, ValueError):
        return 0.5


def _resolve_kernel_size(params: Dict[str, Any]) -> int:
    """形态学核大小（默认 3，最小 1）。"""
    try:
        k = int(params.get("kernel_size", 3))
    except (TypeError, ValueError):
        k = 3
    return max(1, k)


def _resolve_min_area(params: Dict[str, Any]) -> int:
    try:
        m = int(params.get("min_area_pixels", 10))
    except (TypeError, ValueError):
        m = 10
    return max(0, m)


def postprocess(
    mask: np.ndarray,
    confidence: np.ndarray,
    params: Optional[Dict[str, Any]] = None,
    *, valid_mask: np.ndarray | None = None,
) -> np.ndarray:
    """Otsu 阈值 → 形态学开闭 → 最小图斑过滤。

    Parameters
    ----------
    mask : np.ndarray
        2D 0/1 mask。
    confidence : np.ndarray
        2D 0~1 confidence，与 mask 同形状。
    params : dict, optional
        - ``threshold``：``"auto"`` / None → Otsu；显式 number → 直接使用。
        - ``kernel_size``：形态学核大小（默认 3）。
        - ``min_area_pixels``：小于该面积的图斑被丢弃（默认 10）。

    Returns
    -------
    np.ndarray
        处理后的 mask（uint8，0/1）。
    """
    params = params or {}
    mask = mask.astype(np.uint8, copy=False)
    confidence = np.asarray(confidence)
    valid = np.ones(mask.shape, dtype=bool) if valid_mask is None else np.asarray(valid_mask, dtype=bool)
    if valid.shape != mask.shape:
        raise ValueError('有效像元掩膜与检测结果尺寸不一致')
    mask = np.where(valid, mask, 0).astype(np.uint8)

    # 1. Otsu / 显式阈值 → 合并原 mask
    t = _resolve_threshold(params.get("threshold"), confidence[valid], mask)
    if 0.0 <= t <= 1.0:
        # 当 threshold 在 [0, 1] 内时，按 confidence >= t 生成候选；
        # 与原 mask 求 OR 避免「算法本身阈值化后被后处理清空」。
        candidate = (confidence >= t).astype(np.uint8)
        mask = np.maximum(mask, candidate)
    mask[~valid] = 0
    # threshold > 1 表示算法自定尺度（magnitude 之类），不做合并。

    # 2. 形态学：先开后闭，去小斑点、填小孔
    k = _resolve_kernel_size(params)
    structure = np.ones((k, k), dtype=bool)
    mask = ndimage.binary_opening(mask.astype(bool), structure=structure).astype(np.uint8)
    mask[~valid] = 0
    mask = ndimage.binary_closing(mask.astype(bool), structure=structure).astype(np.uint8)
    mask[~valid] = 0

    # 3. 最小图斑过滤（skimage 0.26+ 用 max_size=min_area-1，旧版用 min_size）
    #    新旧语义差异：min_size=N 移除 < N；max_size=N 移除 <= N，
    #    因此用 max_size=min_area-1 与 min_size=min_area 等价。
    min_area = _resolve_min_area(params)
    if min_area > 0:
        try:
            # 优先尝试新签名；新 API 中 max_size=N 等价于旧 min_size=N+1
            cleaned = remove_small_objects(mask.astype(bool),
                                            max_size=max(min_area - 1, 0))
        except TypeError:
            # 旧版 skimage：min_size 直接给阈值
            cleaned = remove_small_objects(mask.astype(bool), min_size=min_area)
        mask = cleaned.astype(np.uint8)

    return mask


def postprocess_pair(
    mask: np.ndarray,
    confidence: np.ndarray,
    params: Optional[Dict[str, Any]] = None,
) -> Tuple[np.ndarray, np.ndarray]:
    """向后兼容：返回 (mask, confidence) 元组（confidence 原样返回）。"""
    return postprocess(mask, confidence, params), confidence.astype(np.float32, copy=False)
