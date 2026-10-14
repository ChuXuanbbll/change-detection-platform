"""算法抽象：DetectionResult + ChangeDetectionAlgorithm Protocol。

约定（与 ``app/routers/detect.py`` 的统一调度保持一致）：
- ``detect()`` 只负责
    1. 读 before / after
    2. 计算差异并落 ``{name}_mask.tif`` (uint8, 0/1)
       与 ``{name}_confidence.tif`` (float32, 0~1)
    3. 返回含 ``mask_path`` / ``confidence_path`` / ``stats`` 的 DetectionResult
- 算法层**不**做：
    - ``postprocess``（Otsu / 形态学 / 最小图斑过滤）— 路由层统一执行
    - 矢量化 → GeoJSON — 路由层调用 ``vectorize.mask_to_geojson`` 生成
- 因此 DetectionResult 用 ``total=False``：``geojson_path`` 由路由层追加
  （这里仅作为可选字段记录在协议里）。
"""
from __future__ import annotations

from pathlib import Path
from typing import Any, Dict, Protocol, TypedDict


class DetectionResult(TypedDict, total=False):
    """单次变化检测产物的统一结构。

    字段均为可选，路由层会按需补齐。
    """

    mask_path: str  # 二值掩膜 GeoTIFF（uint8, 0/1）
    confidence_path: str  # 置信度 GeoTIFF（float32, 0~1）
    geojson_path: str  # 矢量化产物（路由层追加；算法层不生成）
    stats: Dict[str, Any]  # 算法核心统计（含 changed_pixels / change_ratio / mean_confidence / params）


class ChangeDetectionAlgorithm(Protocol):
    """所有变化检测算法须遵循的协议。"""

    name: str
    display_name: str
    # JSON Schema 格式（type: "object" + properties），前端 AlgorithmForm
    # 按 ``schema.properties`` 渲染表单。
    params_schema: Dict[str, Any]
    input_bands: int  # 期望输入波段数

    def detect(
        self,
        before_path: str,
        after_path: str,
        params: Dict[str, Any],
        output_dir: str | Path,
    ) -> DetectionResult:
        ...