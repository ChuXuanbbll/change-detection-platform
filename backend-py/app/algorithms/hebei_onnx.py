"""河北遥感变化检测模型的平台代理适配器。"""
from __future__ import annotations

import os
from pathlib import Path
from typing import Any, Dict

import numpy as np
import rasterio
from rasterio.enums import Resampling
from rasterio.warp import reproject, transform

from app.algorithms.registry import register
from app.utils import model_process


def _default_deployment_dir() -> Path:
    # 优先从项目内查找，便于整个项目搬到另一台电脑；也兼容当前的同级目录布局。
    here = Path(__file__).resolve()
    candidates = [
        here.parents[3] / "model-runtime",
        here.parents[3] / "模型部署",
        here.parents[4] / "模型部署",
    ]
    for candidate in candidates:
        if (candidate / "src" / "predict_city_virtual_patch_pipeline_onnx.py").exists():
            return candidate
    return candidates[0]


@register
class HebeiONNXDetector:
    name = "hebei_onnx"
    display_name = "河北遥感变化检测模型"
    input_bands = 3
    params_schema: Dict[str, Any] = {
        "type": "object",
        "title": "河北遥感变化检测模型参数",
        "properties": {
            "threshold": {"type": "number", "default": 0.5, "minimum": 0, "maximum": 1, "description": "变化概率阈值"},
            "resolution": {"type": "number", "default": 0.8, "minimum": 0.01, "description": "输出分辨率（米）"},
            "provider": {"type": "string", "default": "cuda", "enum": ["cuda"], "description": "模型推理必须使用 GPU"},
        },
        "required": [],
    }

    def detect(self, before_path: str, after_path: str, params: Dict[str, Any], output_dir: str | Path) -> dict:
        if params.get("provider", "cuda") != "cuda":
            raise ValueError("hebei_onnx 模型检测必须使用 GPU（provider=cuda）")
        output_dir = Path(output_dir)
        output_dir.mkdir(parents=True, exist_ok=True)
        from app.utils.progress import Progress
        progress = Progress(output_dir)
        progress("model_loading", 5)
        deployment_dir = Path(os.getenv("CHANGE_DEPLOYMENT_DIR", str(_default_deployment_dir())))
        script = Path(os.getenv("CHANGE_PIPELINE_SCRIPT", str(deployment_dir / "src" / "predict_city_virtual_patch_pipeline_onnx.py")))
        execution = params.get("_execution", {})
        runtime = execution.get("runtime", {})
        model = Path(execution.get("model_path") or os.getenv("CHANGE_MODEL_PATH", str(deployment_dir / "model" / "enforcement_2.onnx")))
        if execution.get("sha256"):
            from app.utils.model_process import verify_hash
            verify_hash(model, execution["sha256"])
        if not script.exists():
            raise FileNotFoundError(f"推理脚本不存在: {script}")
        if not model.exists():
            raise FileNotFoundError(f"模型文件不存在: {model}")

        prob_path = output_dir / f"{self.name}_prob.tif"
        mask_path = output_dir / f"{self.name}_raw_mask.tif"
        work_dir = output_dir / "model_work"
        # resolution is in metres; geographic input coordinates are in degrees.
        # Infer on a metric grid, then return products on the platform input grid.
        with rasterio.open(before_path) as reference:
            if reference.crs is None:
                raise ValueError("输入影像缺少 CRS，无法确定模型分辨率单位")
            profile = reference.profile.copy()
            inference_crs = reference.crs
            if not inference_crs.is_projected or inference_crs.linear_units_factor[1] != 1.0:
                x = (reference.bounds.left + reference.bounds.right) / 2
                y = (reference.bounds.bottom + reference.bounds.top) / 2
                lng, lat = transform(reference.crs, "EPSG:4326", [x], [y])
                zone = max(1, min(60, int((lng[0] + 180) // 6) + 1))
                inference_crs = rasterio.crs.CRS.from_epsg((32600 if lat[0] >= 0 else 32700) + zone)
        runner = [model_process.interpreter()]
        cmd = runner + [
            str(script),
            "--phase_a", str(before_path), "--phase_b", str(after_path),
            "--onnx_model", str(model), "--out_prob", str(prob_path),
            "--out_mask", str(mask_path), "--no_vector",
            "--work_dir", str(work_dir),
            "--progress_dir", str(output_dir / "progress"),
            "--threshold", str(float(params.get("threshold", 0.5))),
            "--resolution", str(float(params.get("resolution", 0.8))),
            "--dst_crs", str(inference_crs),
            "--provider", "cuda", "--device", f"cuda:{runtime.get('gpu_device', 0)}",
            "--batch_size", str(runtime.get("batch_size", 1)),
            "--prefetch_batches", str(runtime.get("prefetch_batches", 2)),
        ]
        for role, name in execution.get("mapping", {}).items():
            flag = {"input_a":"--input_a_name", "input_b":"--input_b_name", "output":"--output_name"}.get(role)
            if flag:
                cmd.extend([flag, name])
        env = os.environ.copy()
        env.setdefault("PYTHONUNBUFFERED", "1")
        completed = model_process.run(cmd, timeout=runtime.get("timeout_seconds", 3600), env=env)
        if completed.returncode != 0:
            log_text = ""
            for log_path in sorted(work_dir.rglob("*.log")) if work_dir.exists() else []:
                try:
                    log_text += f"\n--- {log_path.name} ---\n{log_path.read_text(encoding='utf-8', errors='replace')}"
                except OSError:
                    pass
            output_parts = [completed.stdout, completed.stderr, log_text]
            detail = "\n".join(
                part for part in output_parts
                if isinstance(part, str) and part.strip()
            ).strip()
            detail = (detail or "推理程序执行失败")[-12000:]
            raise RuntimeError(detail)
        if not prob_path.exists() or not mask_path.exists():
            raise RuntimeError("推理程序未生成概率图或掩膜图")

        progress("reprojecting", 85)
        confidence_path = output_dir / f"{self.name}_confidence.tif"
        platform_mask_path = output_dir / f"{self.name}_mask.tif"
        prob = np.zeros((profile["height"], profile["width"]), dtype=np.float32)
        mask = np.zeros(prob.shape, dtype=np.uint8)
        with rasterio.open(prob_path) as src:
            reproject(
                source=src.read(1).astype(np.float32) / 65535.0, destination=prob,
                src_transform=src.transform, src_crs=src.crs,
                dst_transform=profile["transform"], dst_crs=profile["crs"],
                resampling=Resampling.bilinear,
            )
        progress("reprojecting", 87)
        with rasterio.open(mask_path) as src:
            reproject(
                source=(src.read(1) > 0).astype(np.uint8), destination=mask,
                src_transform=src.transform, src_crs=src.crs,
                dst_transform=profile["transform"], dst_crs=profile["crs"],
                resampling=Resampling.nearest,
            )
        profile.update(count=1, dtype="float32", nodata=None)
        with rasterio.open(confidence_path, "w", **profile) as dst:
            dst.write(np.clip(prob, 0, 1), 1)
        profile.update(dtype="uint8")
        with rasterio.open(platform_mask_path, "w", **profile) as dst:
            dst.write(mask, 1)
        return {"mask_path": str(platform_mask_path), "confidence_path": str(confidence_path), "stats": {"algorithm": self.name}}
