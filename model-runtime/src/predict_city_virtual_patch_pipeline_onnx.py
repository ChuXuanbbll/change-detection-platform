import argparse
import json
from contextlib import ExitStack
import glob
import os
import queue
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import uuid
from pathlib import Path

import numpy as np


NORMALIZATION_PRESETS = {
    "imagenet": {
        "mean": (0.485, 0.456, 0.406),
        "std": (0.229, 0.224, 0.225),
    },
}

DEPLOYMENT_DEFAULTS = {
    "norm_type": "imagenet",
    "tile_size": 512,
    "stride": 512,
    "resolution": 0.8,
    "nodata": 0.0,
    "scale": "255",
    "batch_size": 1,
    "provider": "cuda",
    "threshold": 0.5,
    "stitch_mode": "overwrite",
    "compress": "none",
}


class TeeStream:
    """Mirror worker output to the GUI stream and a persistent log file."""

    def __init__(self, *streams):
        self.streams = streams

    def write(self, data):
        for stream in self.streams:
            stream.write(data)
        return len(data)

    def flush(self):
        for stream in self.streams:
            stream.flush()

    def isatty(self):
        return False


def format_duration(seconds):
    seconds = max(0.0, float(seconds))
    total = int(round(seconds))
    hours, rem = divmod(total, 3600)
    minutes, secs = divmod(rem, 60)
    return f"{hours:02d}:{minutes:02d}:{secs:02d}"


def default_onnx_model_path():
    configured = os.getenv("CHANGE_MODEL_PATH")
    if configured:
        return configured
    script_dir = Path(__file__).resolve().parent
    candidates = [
        script_dir / "model" / "model.onnx",
        script_dir.parent / "model" / "model.onnx",
    ]
    for candidate in candidates:
        if candidate.is_file():
            return str(candidate)
    return str(candidates[0])


def parse_args():
    parser = argparse.ArgumentParser(
        description=(
            "ONNX Runtime virtual patch city-scale prediction. This keeps the "
            "crop-predict-stitch semantics without writing patch images to disk."
        )
    )
    parser.add_argument("--phase_a", required=True, help="Before image GeoTIFF/VRT, folder, or txt file list")
    parser.add_argument("--phase_b", required=True, help="After image GeoTIFF/VRT, folder, or txt file list")
    parser.add_argument("--onnx_model", default=default_onnx_model_path(), help="Change-detection ONNX model")
    parser.add_argument("--input_a_name", default=None, help="Optional ONNX input name for phase A")
    parser.add_argument("--input_b_name", default=None, help="Optional ONNX input name for phase B")
    parser.add_argument("--output_name", default=None, help="Optional main-logits ONNX output name")
    parser.add_argument("--output_dir", default=None, help="Deployment mode: automatically create all outputs here")
    parser.add_argument("--output_prefix", default="change", help="Output basename used with --output_dir")
    parser.add_argument("--out_prob", default=None, help="Output uint16 probability GeoTIFF, scaled 0-65535")
    parser.add_argument(
        "--skip_prob",
        action="store_true",
        help="Skip probability part files and the final probability GeoTIFF to reduce multi-GPU disk I/O",
    )
    parser.add_argument("--out_mask", default=None, help="Optional output uint8 binary mask GeoTIFF")
    parser.add_argument("--out_vector", default=None, help="Optional output vector path, usually an ESRI Shapefile .shp")
    parser.add_argument("--no_vector", action="store_true", help="Do not auto-create a vector in deployment mode")
    parser.add_argument("--vector_value", type=int, default=255, help="Mask pixel value to polygonize as change")
    parser.add_argument("--vector_min_area", type=float, default=0.0, help="Drop polygons smaller than this map-unit area")
    parser.add_argument("--vector_simplify", type=float, default=0.0, help="Simplify vector geometry with this map-unit tolerance")
    parser.add_argument("--vector_layer", default="change", help="Output vector layer name")
    parser.add_argument(
        "--fill_holes",
        action=argparse.BooleanOptionalAction,
        default=True,
        help="Fill holes inside change polygons before vector export",
    )
    parser.add_argument("--work_dir", default=None, help="Temporary directory for VRTs and multi-GPU part files")
    parser.add_argument("--keep_parts", action="store_true", help="Keep temporary per-GPU part files for debugging")

    parser.add_argument(
        "--norm_type",
        default=DEPLOYMENT_DEFAULTS["norm_type"],
        choices=["imagenet"],
    )

    parser.add_argument("--tile_size", type=int, default=DEPLOYMENT_DEFAULTS["tile_size"])
    parser.add_argument("--stride", type=int, default=DEPLOYMENT_DEFAULTS["stride"])
    parser.add_argument("--resolution", type=float, default=DEPLOYMENT_DEFAULTS["resolution"])
    parser.add_argument("--dst_crs", default=None)
    parser.add_argument("--bounds", type=float, nargs=4, default=None, help="Output bounds: left bottom right top in dst_crs")
    parser.add_argument("--bounds_mode", default="intersection", choices=["intersection", "union"])
    parser.add_argument("--nodata", type=float, default=DEPLOYMENT_DEFAULTS["nodata"])
    parser.add_argument("--skip_empty", action="store_true")
    parser.add_argument("--min_valid_ratio", type=float, default=0.05)
    parser.add_argument(
        "--scale",
        default=DEPLOYMENT_DEFAULTS["scale"],
        choices=["auto", "255", "10000", "max", "none"],
    )
    parser.add_argument("--max_patches", type=int, default=None, help="Optional tile limit for quick tests")

    parser.add_argument(
        "--batch_size",
        type=int,
        default=DEPLOYMENT_DEFAULTS["batch_size"],
        help="Batch size per GPU worker",
    )
    parser.add_argument("--device", default="cuda:0", help="Single-process device, e.g. cuda:0 or cpu")
    parser.add_argument(
        "--provider",
        default=DEPLOYMENT_DEFAULTS["provider"],
        choices=["auto", "cuda", "cpu"],
    )
    parser.add_argument("--ort_intra_op_threads", type=int, default=0)
    parser.add_argument(
        "--worker_start_delay",
        type=float,
        default=1.0,
        help="Seconds between GPU worker launches to avoid concurrent CUDA/cuDNN initialization spikes",
    )
    parser.add_argument(
        "--prefetch_batches",
        type=int,
        default=2,
        help="Number of prepared batches to prefetch per GPU worker; set 0 to disable",
    )
    parser.add_argument("--progress_dir", default=None, help="Optional structured progress directory")
    parser.add_argument("--num_gpus", type=int, default=1)
    parser.add_argument("--gpu_ids", default=None, help="Comma-separated GPU ids. Defaults to 0..num_gpus-1")
    parser.add_argument(
        "--precision",
        default="fp16",
        choices=["fp32", "fp16"],
        help="Compatibility option; actual input dtype is read from the ONNX model",
    )
    parser.add_argument(
        "--use_threshold",
        action=argparse.BooleanOptionalAction,
        default=True,
        help="Use threshold for mask instead of model argmax",
    )
    parser.add_argument("--threshold", type=float, default=DEPLOYMENT_DEFAULTS["threshold"])
    parser.add_argument(
        "--stitch_mode",
        default=DEPLOYMENT_DEFAULTS["stitch_mode"],
        choices=["overwrite", "max"],
    )
    parser.add_argument("--debug_stats", action="store_true")
    parser.add_argument("--compress", default=DEPLOYMENT_DEFAULTS["compress"])

    parser.add_argument("--worker", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--worker_rank", type=int, default=0, help=argparse.SUPPRESS)
    parser.add_argument("--worker_world", type=int, default=1, help=argparse.SUPPRESS)
    return parser.parse_args()


def require_rasterio():
    try:
        import rasterio
        from rasterio.enums import Resampling
        from rasterio.transform import from_origin
        from rasterio.vrt import WarpedVRT
        from rasterio.warp import transform_bounds
        from rasterio.windows import Window
    except ModuleNotFoundError as exc:
        raise ModuleNotFoundError("This script needs rasterio. Install with conda install -c conda-forge rasterio") from exc
    return rasterio, Window, WarpedVRT, Resampling, from_origin, transform_bounds


def collect_rasters(path):
    if os.path.isdir(path):
        files = []
        for ext in ("*.tif", "*.tiff", "*.TIF", "*.TIFF"):
            files.extend(glob.glob(os.path.join(path, ext)))
        return sorted(files)
    if str(path).lower().endswith(".txt"):
        with open(path, "r", encoding="utf-8") as f:
            return [line.strip() for line in f if line.strip()]
    return None


def prepare_raster_path(path, name, work_dir):
    files = collect_rasters(path)
    if files is None:
        return path
    if not files:
        raise ValueError(f"No tif files found from: {path}")
    list_path = Path(work_dir) / f"{name}_files.txt"
    vrt_path = Path(work_dir) / f"{name}.vrt"
    with list_path.open("w", encoding="utf-8") as f:
        for file_path in files:
            f.write(str(file_path) + "\n")
    cmd = [gdal_command("gdalbuildvrt"), "-overwrite", "-input_file_list", str(list_path), str(vrt_path)]
    run_subprocess(cmd, "Build VRT")
    return str(vrt_path)


def merge_bounds(bounds_a, bounds_b, mode):
    if mode == "union":
        return (
            min(bounds_a[0], bounds_b[0]),
            min(bounds_a[1], bounds_b[1]),
            max(bounds_a[2], bounds_b[2]),
            max(bounds_a[3], bounds_b[3]),
        )
    return (
        max(bounds_a[0], bounds_b[0]),
        max(bounds_a[1], bounds_b[1]),
        min(bounds_a[2], bounds_b[2]),
        min(bounds_a[3], bounds_b[3]),
    )


def compute_output_grid(args, raw_a, raw_b, from_origin, transform_bounds):
    dst_crs = args.dst_crs or raw_a.crs
    if args.bounds is None:
        bounds_a = transform_bounds(raw_a.crs, dst_crs, *raw_a.bounds, densify_pts=21)
        bounds_b = transform_bounds(raw_b.crs, dst_crs, *raw_b.bounds, densify_pts=21)
        left, bottom, right, top = merge_bounds(bounds_a, bounds_b, args.bounds_mode)
    else:
        left, bottom, right, top = args.bounds
    if right <= left or top <= bottom:
        raise ValueError("Invalid output bounds. Check CRS, image coverage, or --bounds_mode.")
    resolution = args.resolution
    if resolution is None:
        resolution = min(abs(raw_a.res[0]), abs(raw_a.res[1]), abs(raw_b.res[0]), abs(raw_b.res[1]))
    width = int(np.ceil((right - left) / resolution))
    height = int(np.ceil((top - bottom) / resolution))
    transform = from_origin(left, top, resolution, resolution)
    return dst_crs, resolution, width, height, transform


def compute_starts(length, tile_size, stride):
    if length <= tile_size:
        return [0]
    starts = list(range(0, length - tile_size + 1, stride))
    last = length - tile_size
    if starts[-1] != last:
        starts.append(last)
    return starts


def scale_to_uint8(arr, mode):
    arr = arr.astype(np.float32, copy=False)
    if mode in ("none", "255"):
        return np.clip(arr, 0, 255).astype(np.uint8)
    if mode == "10000":
        return np.clip(arr / 10000.0 * 255.0, 0, 255).astype(np.uint8)
    if mode == "max":
        max_value = max(float(np.nanmax(arr)), 1.0)
        return np.clip(arr / max_value * 255.0, 0, 255).astype(np.uint8)
    if np.nanmax(arr) > 255:
        return np.clip(arr / 10000.0 * 255.0, 0, 255).astype(np.uint8)
    return np.clip(arr, 0, 255).astype(np.uint8)


def read_rgb_uint8(src, window, tile_size, nodata, scale_mode):
    arr = src.read(indexes=[1, 2, 3], window=window, fill_value=nodata)
    valid = np.any(arr != nodata, axis=0)
    arr = np.transpose(arr, (1, 2, 0))
    if arr.shape[0] != tile_size or arr.shape[1] != tile_size:
        padded = np.zeros((tile_size, tile_size, 3), dtype=arr.dtype)
        padded[: arr.shape[0], : arr.shape[1]] = arr
        arr = padded
    return scale_to_uint8(arr, scale_mode), valid


def normalize_image(image, norm_type):
    preset = NORMALIZATION_PRESETS[norm_type]
    mean = np.asarray(preset["mean"], dtype=np.float32).reshape(1, 1, 3)
    std = np.asarray(preset["std"], dtype=np.float32).reshape(1, 1, 3)
    image = image.astype(np.float32) / 255.0
    image = (image - mean) / std
    return np.transpose(image, (2, 0, 1)).astype(np.float32, copy=False)


def ort_type_to_numpy(type_name):
    mapping = {
        "tensor(float)": np.float32,
        "tensor(float16)": np.float16,
    }
    if type_name not in mapping:
        raise TypeError(f"Unsupported ONNX input type: {type_name}. Expected float32 or float16.")
    return mapping[type_name]


def fixed_dimension(value):
    return int(value) if isinstance(value, int) and value > 0 else None


class ONNXChangeDetector:
    def __init__(self, args):
        try:
            import onnxruntime as ort
        except ModuleNotFoundError as exc:
            raise ModuleNotFoundError(
                "This script needs onnxruntime-gpu for CUDA or onnxruntime for CPU."
            ) from exc

        session_options = ort.SessionOptions()
        session_options.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
        if args.ort_intra_op_threads > 0:
            session_options.intra_op_num_threads = int(args.ort_intra_op_threads)

        wants_cuda = args.provider == "cuda" or (args.provider == "auto" and "cuda" in args.device.lower())
        if wants_cuda and hasattr(ort, "preload_dlls"):
            try:
                if getattr(sys, "frozen", False):
                    runtime_root = Path(getattr(sys, "_MEIPASS", Path(sys.executable).resolve().parent))
                    ort.preload_dlls(directory=str(runtime_root))
                else:
                    # start-all.ps1 may discover a CUDA 12/cuDNN 9 runtime
                    # outside this Python environment (for example a Conda
                    # PyTorch environment). Passing that directory lets ORT
                    # load the exact DLL set instead of probing incompatible
                    # system CUDA versions first.
                    runtime_dir = os.getenv("CHANGE_CUDA_DLL_DIR", "").strip()
                    ort.preload_dlls(directory=runtime_dir)
            except Exception as exc:
                print(f"ONNX Runtime CUDA library preload warning: {exc}", flush=True)

        available = ort.get_available_providers()
        if wants_cuda:
            if "CUDAExecutionProvider" not in available:
                if args.provider == "cuda":
                    raise RuntimeError(
                        "CUDAExecutionProvider is unavailable. Install onnxruntime-gpu and check CUDA libraries."
                    )
                print("CUDAExecutionProvider unavailable; falling back to CPUExecutionProvider", flush=True)
                providers = ["CPUExecutionProvider"]
            else:
                device_id = 0
                if not args.worker and ":" in args.device:
                    device_id = int(args.device.rsplit(":", 1)[1])
                cuda_options = {
                    "device_id": device_id,
                    "cudnn_conv_algo_search": "HEURISTIC",
                    "cudnn_conv_use_max_workspace": "0",
                    "arena_extend_strategy": "kSameAsRequested",
                    "do_copy_in_default_stream": "1",
                    "use_tf32": "1",
                }
                providers = [
                    ("CUDAExecutionProvider", cuda_options),
                    "CPUExecutionProvider",
                ]
        else:
            providers = ["CPUExecutionProvider"]

        self.session = ort.InferenceSession(
            args.onnx_model,
            sess_options=session_options,
            providers=providers,
        )
        active_providers = self.session.get_providers()
        if wants_cuda and "CUDAExecutionProvider" not in active_providers:
            message = (
                "CUDAExecutionProvider was requested but did not start. Check the ONNX Runtime, "
                "CUDA and cuDNN major-version match. The model would otherwise run on CPU."
            )
            if args.provider == "cuda":
                raise RuntimeError(message)
            print(f"Warning: {message}", flush=True)
        if wants_cuda and "CUDAExecutionProvider" in active_providers:
            disable_fallback = getattr(self.session, "disable_fallback", None)
            if callable(disable_fallback):
                disable_fallback()
        self.inputs = self.session.get_inputs()
        self.outputs = self.session.get_outputs()
        self.output_name = args.output_name or self.outputs[0].name
        output_names = {item.name for item in self.outputs}
        if self.output_name not in output_names:
            raise ValueError(f"Unknown --output_name '{self.output_name}'. Available: {sorted(output_names)}")

        if len(self.inputs) == 1:
            self.input_mode = "six_channel"
            self.input_a = self.inputs[0]
            self.input_b = None
            if args.input_a_name and args.input_a_name != self.input_a.name:
                raise ValueError(f"The model has one input named '{self.input_a.name}', not '{args.input_a_name}'.")
        elif len(self.inputs) == 2:
            self.input_mode = "two_input"
            by_name = {item.name: item for item in self.inputs}
            self.input_a = by_name.get(args.input_a_name) if args.input_a_name else self.inputs[0]
            self.input_b = by_name.get(args.input_b_name) if args.input_b_name else self.inputs[1]
            if self.input_a is None or self.input_b is None:
                raise ValueError(f"Unknown ONNX input name. Available: {sorted(by_name)}")
        else:
            raise ValueError(
                f"Expected one 6-channel input or two 3-channel inputs, got {len(self.inputs)} inputs."
            )

        self.input_dtype = ort_type_to_numpy(self.input_a.type)
        if self.input_b is not None and ort_type_to_numpy(self.input_b.type) != self.input_dtype:
            raise TypeError("Phase A and phase B ONNX inputs must have the same dtype.")
        self.fixed_batch = fixed_dimension(self.input_a.shape[0])
        self.debug_stats = bool(args.debug_stats)
        self.debug_printed = False
        self._retry_notice_printed = False
        self._validate_spatial_shape(args.tile_size)

        print(
            f"ONNX loaded: providers={self.session.get_providers()}, mode={self.input_mode}, "
            f"inputs={[item.name for item in self.inputs]}, output={self.output_name}, "
            f"dtype={np.dtype(self.input_dtype).name}, fixed_batch={self.fixed_batch}",
            flush=True,
        )

    def _validate_spatial_shape(self, tile_size):
        for item in [self.input_a, self.input_b]:
            if item is None or len(item.shape) != 4:
                continue
            expected_channels = 6 if self.input_mode == "six_channel" else 3
            fixed_channels = fixed_dimension(item.shape[1])
            if fixed_channels is not None and fixed_channels != expected_channels:
                raise ValueError(
                    f"ONNX input '{item.name}' has {fixed_channels} channels; expected {expected_channels}."
                )
            fixed_h = fixed_dimension(item.shape[-2])
            fixed_w = fixed_dimension(item.shape[-1])
            if fixed_h is not None and fixed_h != tile_size:
                raise ValueError(f"ONNX input height is {fixed_h}, but --tile_size is {tile_size}.")
            if fixed_w is not None and fixed_w != tile_size:
                raise ValueError(f"ONNX input width is {fixed_w}, but --tile_size is {tile_size}.")

    @staticmethod
    def _is_retryable_cuda_error(exc):
        message = str(exc).upper()
        return any(
            marker in message
            for marker in (
                "CUDNN",
                "CUDA",
                "CUBLAS",
                "EP_FAIL",
                "ALLOC",
                "OUT OF MEMORY",
            )
        )

    def _run_array(self, x):
        real_batch = x.shape[0]
        if self.fixed_batch is not None and real_batch != self.fixed_batch:
            if real_batch > self.fixed_batch:
                raise ValueError(
                    f"Inference batch {real_batch} exceeds fixed ONNX batch {self.fixed_batch}."
                )
            padding = np.repeat(x[-1:], self.fixed_batch - real_batch, axis=0)
            x = np.concatenate([x, padding], axis=0)
        if self.input_mode == "six_channel":
            feed = {self.input_a.name: x}
        else:
            feed = {
                self.input_a.name: x[:, :3],
                self.input_b.name: x[:, 3:],
            }
        try:
            logits = self.session.run([self.output_name], feed)[0]
        except Exception as exc:
            can_split = (
                self.fixed_batch is None
                and real_batch > 1
                and self._is_retryable_cuda_error(exc)
            )
            if not can_split:
                raise RuntimeError(
                    "ONNX CUDA inference failed even at the smallest usable batch. "
                    "Set the GUI per-GPU batch size to 1 and verify that the package "
                    "contains one complete, matching CUDA 12/cuDNN 9 runtime. "
                    f"Original error: {exc}"
                ) from exc
            midpoint = real_batch // 2
            if not self._retry_notice_printed:
                print(
                    "CUDA/cuDNN rejected the current batch; automatically splitting "
                    f"batch {real_batch} into smaller batches.",
                    flush=True,
                )
                self._retry_notice_printed = True
            left = self._run_array(x[:midpoint])
            right = self._run_array(x[midpoint:])
            return np.concatenate((left, right), axis=0)

        return logits[:real_batch]

    def infer(self, batch_inputs):
        x = np.stack(batch_inputs, axis=0).astype(self.input_dtype, copy=False)
        real_batch = x.shape[0]
        logits = self._run_array(x)
        if logits.ndim != 4:
            raise ValueError(f"Expected NCHW logits, got shape {logits.shape}")
        logits = logits[:real_batch].astype(np.float32, copy=False)
        if self.debug_stats and not self.debug_printed:
            print(
                "DEBUG_ONNX_LOGITS "
                f"shape={logits.shape} min={logits.min():.6f} "
                f"max={logits.max():.6f} mean={logits.mean():.6f}",
                flush=True,
            )
            self.debug_printed = True
        return logits


def logits_to_probs_and_masks(logits, use_threshold, threshold):
    if logits.shape[1] == 1:
        clipped = np.clip(logits[:, 0], -80.0, 80.0)
        probs = 1.0 / (1.0 + np.exp(-clipped))
        preds = (probs >= threshold).astype(np.uint8)
        return probs, preds
    if logits.shape[1] < 2:
        raise ValueError(f"Unsupported logits shape: {logits.shape}")
    shifted = logits - np.max(logits, axis=1, keepdims=True)
    exp_logits = np.exp(shifted)
    probs_all = exp_logits / np.sum(exp_logits, axis=1, keepdims=True)
    probs = probs_all[:, 1]
    if use_threshold:
        preds = (probs >= threshold).astype(np.uint8)
    else:
        preds = np.argmax(logits, axis=1).astype(np.uint8)
    return probs, preds


def batch_to_probs_and_masks(model, batch_inputs, use_threshold, threshold):
    logits = model.infer(batch_inputs)
    return logits_to_probs_and_masks(logits, use_threshold, threshold)


def make_output_profile(width, height, crs, transform, dtype, compress):
    profile = {
        "driver": "GTiff",
        "height": height,
        "width": width,
        "count": 1,
        "crs": crs,
        "transform": transform,
        "dtype": dtype,
        "nodata": None,
        "BIGTIFF": "YES",
        "tiled": True,
        "blockxsize": 256,
        "blockysize": 256,
    }
    if compress and str(compress).lower() != "none":
        profile["compress"] = compress
    return profile


def write_tile(dst, Window, arr, x, y, width, height, mode):
    y1 = min(y + arr.shape[0], height)
    x1 = min(x + arr.shape[1], width)
    arr = arr[: y1 - y, : x1 - x]
    window = Window(x, y, x1 - x, y1 - y)
    if mode == "max":
        old = dst.read(1, window=window)
        if old.shape == arr.shape:
            arr = np.maximum(old, arr)
    dst.write(arr, 1, window=window)


def make_batch_from_tiles(src_a, src_b, tiles, args, Window, effective_batch_size):
    batch_inputs, batch_meta = [], []
    for _, y, x in tiles:
        window = Window(x, y, args.tile_size, args.tile_size)
        img_a, valid_a = read_rgb_uint8(src_a, window, args.tile_size, args.nodata, args.scale)
        img_b, valid_b = read_rgb_uint8(src_b, window, args.tile_size, args.nodata, args.scale)
        valid_ratio = min(float(valid_a.mean()), float(valid_b.mean()))
        if args.skip_empty and valid_ratio < args.min_valid_ratio:
            continue

        input_a = normalize_image(img_a, args.norm_type)
        input_b = normalize_image(img_b, args.norm_type)
        batch_inputs.append(np.concatenate((input_a, input_b), axis=0))
        batch_meta.append((y, x))

        if len(batch_inputs) == effective_batch_size:
            yield batch_inputs, batch_meta
            batch_inputs, batch_meta = [], []

    if batch_inputs:
        yield batch_inputs, batch_meta


def iter_prefetched_batches(src_a, src_b, tiles, args, Window, effective_batch_size):
    prefetch_batches = int(getattr(args, "prefetch_batches", 0) or 0)
    if prefetch_batches <= 0:
        yield from make_batch_from_tiles(src_a, src_b, tiles, args, Window, effective_batch_size)
        return

    batch_queue = queue.Queue(maxsize=max(1, prefetch_batches))
    sentinel = object()

    def producer():
        try:
            for item in make_batch_from_tiles(src_a, src_b, tiles, args, Window, effective_batch_size):
                batch_queue.put(item)
        except BaseException as exc:
            batch_queue.put(exc)
        finally:
            batch_queue.put(sentinel)

    thread = threading.Thread(target=producer, name="patch-prefetch", daemon=True)
    thread.start()

    while True:
        item = batch_queue.get()
        if item is sentinel:
            break
        if isinstance(item, BaseException):
            raise item
        yield item

    thread.join()


def report_worker_progress(args, done, total):
    directory = getattr(args, "progress_dir", None)
    if not directory:
        return
    now = time.monotonic()
    previous = getattr(args, "_progress_write", None)
    if previous is not None and done != total and now - previous < .25:
        return
    try:
        path = Path(directory) / f"worker-{args.worker_rank}.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        temporary = path.with_suffix(".tmp")
        temporary.write_text(json.dumps({"rank": args.worker_rank, "world": args.worker_world,
                                        "done": done, "total": total}), encoding="utf-8")
        os.replace(temporary, path)
        args._progress_write = now
    except OSError:
        pass


def run_worker(args):
    rasterio, Window, WarpedVRT, Resampling, from_origin, transform_bounds = require_rasterio()
    model = ONNXChangeDetector(args)
    effective_batch_size = int(args.batch_size)
    if model.fixed_batch is not None:
        if effective_batch_size != model.fixed_batch:
            print(
                f"ONNX model has fixed batch={model.fixed_batch}; overriding --batch_size={effective_batch_size}",
                flush=True,
            )
        effective_batch_size = model.fixed_batch

    with rasterio.open(args.phase_a) as raw_a, rasterio.open(args.phase_b) as raw_b:
        if raw_a.count < 3 or raw_b.count < 3:
            raise ValueError("Both input images must have at least 3 bands.")
        if raw_a.crs is None or raw_b.crs is None:
            raise ValueError("Both input images must have CRS information.")
        crs, resolution, width, height, transform = compute_output_grid(args, raw_a, raw_b, from_origin, transform_bounds)
        vrt_kwargs = dict(
            crs=crs,
            transform=transform,
            width=width,
            height=height,
            resampling=Resampling.bilinear,
            src_nodata=args.nodata,
            nodata=args.nodata,
        )

        y_starts = compute_starts(height, args.tile_size, args.stride)
        x_starts = compute_starts(width, args.tile_size, args.stride)
        tiles = [(idx, y, x) for idx, (y, x) in enumerate((yy, xx) for yy in y_starts for xx in x_starts)]
        if args.max_patches is not None:
            tiles = tiles[: args.max_patches]
        tiles = [item for item in tiles if item[0] % args.worker_world == args.worker_rank]
        total = len(tiles)
        report_worker_progress(args, 0, total)

        print(
            f"Output grid: width={width}, height={height}, resolution={resolution}; "
            f"worker={args.worker_rank}/{args.worker_world}; tiles={total}",
            flush=True,
        )
        output_path = args.out_mask if args.skip_prob else args.out_prob
        if output_path is None:
            raise ValueError("At least one of probability or mask output must be enabled.")
        Path(output_path).parent.mkdir(parents=True, exist_ok=True)
        prob_profile = (
            make_output_profile(width, height, crs, transform, "uint16", args.compress)
            if not args.skip_prob else None
        )
        mask_profile = make_output_profile(width, height, crs, transform, "uint8", args.compress) if args.out_mask else None

        with WarpedVRT(raw_a, **vrt_kwargs) as src_a, WarpedVRT(raw_b, **vrt_kwargs) as src_b:
            prob_dst = rasterio.open(args.out_prob, "w+", **prob_profile) if not args.skip_prob else None
            mask_dst = rasterio.open(args.out_mask, "w+", **mask_profile) if args.out_mask else None
            try:
                printed_debug = False
                done = 0
                for batch_inputs, batch_meta in iter_prefetched_batches(
                    src_a, src_b, tiles, args, Window, effective_batch_size
                ):
                    done += flush_batch(
                        args, model, batch_inputs, batch_meta, Window,
                        prob_dst, mask_dst, width, height
                    )
                    if args.debug_stats and not printed_debug:
                        print_debug_stats(batch_inputs)
                        printed_debug = True
                    report_worker_progress(args, done, total)
                    print(f"worker {args.worker_rank}: processed {done}/{total}", flush=True)
            finally:
                if mask_dst is not None:
                    mask_dst.close()
                if prob_dst is not None:
                    prob_dst.close()


def flush_batch(args, model, batch_inputs, batch_meta, Window, prob_dst, mask_dst, width, height):
    probs, preds = batch_to_probs_and_masks(
        model,
        batch_inputs,
        args.use_threshold,
        args.threshold,
    )
    for prob, pred, (y, x) in zip(probs, preds, batch_meta):
        if prob_dst is not None:
            prob_arr = np.clip(prob * 65535.0, 0, 65535).astype(np.uint16)
            write_tile(prob_dst, Window, prob_arr, x, y, width, height, args.stitch_mode)
        if mask_dst is not None:
            mask_arr = (pred.astype(np.uint8) > 0).astype(np.uint8) * 255
            write_tile(mask_dst, Window, mask_arr, x, y, width, height, args.stitch_mode)
    return len(batch_inputs)


def print_debug_stats(batch_inputs):
    x = np.stack(batch_inputs, axis=0).astype(np.float32, copy=False)
    print(
        "DEBUG_INPUT "
        f"x_min={x.min():.6f} x_max={x.max():.6f} x_mean={x.mean():.6f}",
        flush=True,
    )


def resolve_gpu_ids(args):
    if args.gpu_ids:
        gpu_ids = [item.strip() for item in args.gpu_ids.split(",") if item.strip()]
    else:
        gpu_ids = [str(i) for i in range(args.num_gpus)]
    if len(gpu_ids) != args.num_gpus:
        raise ValueError("--gpu_ids count must match --num_gpus")
    return gpu_ids


def build_worker_cmd(args, rank, world, out_prob, out_mask):
    packaged_launcher = os.getenv("CHANGE_WORKER_EXECUTABLE")
    if packaged_launcher:
        worker_entry = [packaged_launcher, "--worker-entry"]
    else:
        worker_entry = [sys.executable, os.path.abspath(__file__)]
    cmd = [
        *worker_entry,
        "--worker",
        "--worker_rank",
        str(rank),
        "--worker_world",
        str(world),
        "--phase_a",
        args.phase_a,
        "--phase_b",
        args.phase_b,
        "--onnx_model",
        args.onnx_model,
        "--out_prob",
        str(out_prob),
        "--norm_type",
        args.norm_type,
        "--tile_size",
        str(args.tile_size),
        "--stride",
        str(args.stride),
        "--nodata",
        str(args.nodata),
        "--scale",
        args.scale,
        "--batch_size",
        str(args.batch_size),
        "--device",
        "cuda:0",
        "--provider",
        args.provider,
        "--ort_intra_op_threads",
        str(args.ort_intra_op_threads),
        "--worker_start_delay",
        str(args.worker_start_delay),
        "--prefetch_batches",
        str(args.prefetch_batches),
        "--precision",
        args.precision,
        "--threshold",
        str(args.threshold),
        "--stitch_mode",
        args.stitch_mode,
        "--bounds_mode",
        args.bounds_mode,
        "--min_valid_ratio",
        str(args.min_valid_ratio),
        "--compress",
        args.compress,
    ]
    if out_mask is not None:
        cmd += ["--out_mask", str(out_mask)]
    if args.skip_prob:
        cmd.append("--skip_prob")
    if args.input_a_name:
        cmd += ["--input_a_name", args.input_a_name]
    if args.input_b_name:
        cmd += ["--input_b_name", args.input_b_name]
    if args.output_name:
        cmd += ["--output_name", args.output_name]
    if args.resolution is not None:
        cmd += ["--resolution", str(args.resolution)]
    if args.dst_crs:
        cmd += ["--dst_crs", args.dst_crs]
    if args.bounds is not None:
        cmd += ["--bounds", *[str(v) for v in args.bounds]]
    if args.skip_empty:
        cmd.append("--skip_empty")
    if args.use_threshold:
        cmd.append("--use_threshold")
    else:
        cmd.append("--no-use_threshold")
    if args.debug_stats:
        cmd.append("--debug_stats")
    if getattr(args, "progress_dir", None):
        cmd += ["--progress_dir", args.progress_dir]
    if args.max_patches is not None:
        cmd += ["--max_patches", str(args.max_patches)]
    return cmd


def merge_part_files(part_paths, out_path, dtype, compress):
    rasterio, Window, _, _, _, _ = require_rasterio()
    if not part_paths:
        raise ValueError(f"No part files were provided for {out_path}")
    with ExitStack() as stack:
        sources = [stack.enter_context(rasterio.open(path)) for path in part_paths]
        first = sources[0]
        profile = first.profile.copy()
        profile.update(dtype=dtype, BIGTIFF="YES")
        if compress and str(compress).lower() != "none":
            profile["compress"] = compress
        else:
            profile.pop("compress", None)
        Path(out_path).parent.mkdir(parents=True, exist_ok=True)
        with rasterio.open(out_path, "w", **profile) as dst:
            for _, window in first.block_windows(1):
                merged = first.read(1, window=window)
                for src in sources[1:]:
                    merged = np.maximum(merged, src.read(1, window=window))
                dst.write(merged.astype(dtype), 1, window=window)
    print(f"Saved: {out_path}", flush=True)


def run_multi_gpu(args, phase_a, phase_b):
    if args.num_gpus <= 1:
        log_dir = Path(args.work_dir) / "logs" if args.work_dir else Path(tempfile.gettempdir()) / "ChangeDetectionApp" / "logs"
        log_dir.mkdir(parents=True, exist_ok=True)
        log_path = log_dir / "worker_000.log"
        original_stdout = sys.stdout
        original_stderr = sys.stderr
        with log_path.open("w", encoding="utf-8") as log_file:
            sys.stdout = TeeStream(original_stdout, log_file)
            sys.stderr = TeeStream(original_stderr, log_file)
            try:
                run_worker(args)
            finally:
                sys.stdout = original_stdout
                sys.stderr = original_stderr
        print(f"Worker log: {log_path}", flush=True)
        return
    if args.stride < args.tile_size and args.stitch_mode == "overwrite":
        print(
            "Warning: multi-GPU overwrite with overlapping windows is not strictly identical "
            "to single-process patch overwrite. Use --stride == --tile_size for exact crop-like output.",
            flush=True,
        )

    gpu_ids = resolve_gpu_ids(args)
    tmp_owner = None
    if args.work_dir:
        work_dir = Path(args.work_dir)
        work_dir.mkdir(parents=True, exist_ok=True)
    else:
        tmp_owner = tempfile.TemporaryDirectory(prefix="virtual_patch_")
        work_dir = Path(tmp_owner.name)
    part_dir = work_dir / "parts"
    log_dir = work_dir / "logs"
    part_dir.mkdir(parents=True, exist_ok=True)
    log_dir.mkdir(parents=True, exist_ok=True)

    try:
        args.phase_a = phase_a
        args.phase_b = phase_b
        processes = []
        prob_parts, mask_parts = [], []
        for rank, gpu_id in enumerate(gpu_ids):
            part_prob = part_dir / f"part_{rank:03d}_prob.tif"
            part_mask = part_dir / f"part_{rank:03d}_mask.tif" if args.out_mask else None
            if not args.skip_prob:
                prob_parts.append(part_prob)
            if part_mask is not None:
                mask_parts.append(part_mask)
            cmd = build_worker_cmd(args, rank, args.num_gpus, part_prob, part_mask)
            env = os.environ.copy()
            env["CUDA_VISIBLE_DEVICES"] = gpu_id
            log_path = log_dir / f"worker_{rank:03d}.log"
            log_file = log_path.open("w", encoding="utf-8")
            primary_part = part_mask if args.skip_prob else part_prob
            print(f"launch GPU {gpu_id}: {primary_part}, log={log_path}", flush=True)
            proc = subprocess.Popen(cmd, env=env, stdout=log_file, stderr=subprocess.STDOUT)
            processes.append((rank, proc, log_file, log_path))
            if rank + 1 < len(gpu_ids) and args.worker_start_delay > 0:
                time.sleep(args.worker_start_delay)

        worker_wait_start = time.perf_counter()
        failed = []
        for rank, proc, log_file, log_path in processes:
            ret = proc.wait()
            log_file.close()
            if ret != 0:
                failed.append((rank, ret, log_path))
        if failed:
            for rank, ret, log_path in failed:
                print(
                    f"\n========== worker {rank} failed (exit {ret}) ==========" ,
                    flush=True,
                )
                try:
                    lines = log_path.read_text(
                        encoding="utf-8", errors="replace"
                    ).splitlines()
                    tail = lines[-80:]
                    if tail:
                        print("\n".join(tail), flush=True)
                    else:
                        print(f"Worker log is empty: {log_path}", flush=True)
                except OSError as exc:
                    print(
                        f"Could not read worker log {log_path}: {exc}",
                        flush=True,
                    )
            detail = ", ".join(f"worker {rank} exit {ret}, log={log}" for rank, ret, log in failed)
            raise RuntimeError(f"Virtual patch workers failed: {detail}")
        print(f"GPU worker time: {format_duration(time.perf_counter() - worker_wait_start)}", flush=True)

        if not args.skip_prob:
            merge_start = time.perf_counter()
            print("Merging probability parts", flush=True)
            merge_part_files(prob_parts, args.out_prob, "uint16", args.compress)
            print(f"Probability merge time: {format_duration(time.perf_counter() - merge_start)}", flush=True)
        if args.out_mask:
            merge_start = time.perf_counter()
            print("Merging mask parts", flush=True)
            merge_part_files(mask_parts, args.out_mask, "uint8", args.compress)
            print(f"Mask merge time: {format_duration(time.perf_counter() - merge_start)}", flush=True)
        if args.keep_parts:
            print(f"Kept part files: {part_dir}", flush=True)
        print(f"Worker logs: {log_dir}", flush=True)
    finally:
        if tmp_owner is not None:
            tmp_owner.cleanup()
        elif not args.keep_parts:
            shutil.rmtree(part_dir, ignore_errors=True)



def find_osgeo_root():
    roots = []
    if os.environ.get("OSGEO4W_ROOT"):
        roots.append(Path(os.environ["OSGEO4W_ROOT"]))
    for command in ("gdalbuildvrt", "ogr2ogr"):
        found = shutil.which(command)
        if found:
            roots.append(Path(found).resolve().parent.parent)
    roots.extend([
        Path(r"C:\OSGeo4W"),
        Path.home() / "AppData" / "Local" / "Programs" / "OSGeo4W",
    ])
    for root in roots:
        if (root / "bin").is_dir():
            return root
    return None


def gdal_command(name):
    root = find_osgeo_root()
    candidates = []
    if root:
        candidates.append(root / "bin" / f"{name}.exe")
        candidates.append(root / "bin" / name)
    found = shutil.which(name)
    if found:
        candidates.append(Path(found))
    for candidate in candidates:
        if candidate.is_file():
            return str(candidate)
    return name


def find_osgeo_python_home(root=None):
    root = root or find_osgeo_root()
    if not root:
        return None
    for name in ("Python313", "Python312", "Python311", "Python310"):
        candidate = root / "apps" / name
        if (candidate / "Lib" / "encodings").is_dir():
            return candidate
    return None


def find_osgeo_python():
    root = find_osgeo_root()
    if not root:
        return None
    python_home = find_osgeo_python_home(root)
    candidates = []
    if python_home:
        candidates.append(python_home / "python.exe")
    candidates.extend([
        root / "bin" / "python.exe",
        root / "bin" / "python3.exe",
    ])
    for candidate in candidates:
        if candidate.is_file():
            return str(candidate)
    return None


def find_gdal_polygonize_script():
    root = find_osgeo_root()
    candidates = []
    if root:
        candidates.extend([
            root / "apps" / "Python312" / "Scripts" / "gdal_polygonize.py",
            root / "apps" / "Python311" / "Scripts" / "gdal_polygonize.py",
            root / "apps" / "Python310" / "Scripts" / "gdal_polygonize.py",
            root / "apps" / "Python312" / "Lib" / "site-packages" / "osgeo_utils" / "gdal_polygonize.py",
            root / "apps" / "Python311" / "Lib" / "site-packages" / "osgeo_utils" / "gdal_polygonize.py",
            root / "apps" / "Python310" / "Lib" / "site-packages" / "osgeo_utils" / "gdal_polygonize.py",
        ])
    found = shutil.which("gdal_polygonize.py")
    if found:
        candidates.append(Path(found))
    for candidate in candidates:
        if candidate.is_file():
            return str(candidate)
    return None


def gdal_polygonize_command():
    script = find_gdal_polygonize_script()
    if os.name == "nt":
        python_exe = find_osgeo_python()
        if script and python_exe:
            return [python_exe, script]
        if python_exe:
            return [python_exe, "-m", "osgeo_utils.gdal_polygonize"]
        raise RuntimeError(
            "Could not find OSGeo4W Python for gdal_polygonize.py. "
            "Set OSGEO4W_ROOT to the OSGeo4W install directory."
        )
    if script:
        return [script]
    return [sys.executable, "-m", "osgeo_utils.gdal_polygonize"]


def gdal_subprocess_env():
    env = os.environ.copy()
    # A frozen PyInstaller process carries Python and bootloader variables that
    # can make an external OSGeo4W Python load the bundled runtime by mistake.
    for name in (
        "PYTHONHOME",
        "PYTHONPATH",
        "PYTHONEXECUTABLE",
        "PYTHONSTARTUP",
        "PYTHONUSERBASE",
        "__PYVENV_LAUNCHER__",
        "_MEIPASS2",
    ):
        env.pop(name, None)
    for name in list(env):
        if name.startswith("_PYI_"):
            env.pop(name, None)
    root = find_osgeo_root()
    if root:
        python_home = find_osgeo_python_home(root)
        osgeo_paths = [
            str(root / "bin"),
            str(root / "apps" / "Python312" / "Scripts"),
            str(root / "apps" / "Python311" / "Scripts"),
            str(root / "apps" / "Python310" / "Scripts"),
        ]
        if python_home:
            osgeo_paths[1:1] = [str(python_home), str(python_home / "DLLs")]
            env["PYTHONHOME"] = str(python_home)
        env["OSGEO4W_ROOT"] = str(root)
        env["PATH"] = os.pathsep.join(osgeo_paths + [env.get("PATH", "")])
        env["GDAL_DATA"] = str(root / "share" / "gdal")
        env["PROJ_LIB"] = str(root / "share" / "proj")
        env["PROJ_DATA"] = str(root / "share" / "proj")
    env.setdefault("PYTHONUTF8", "1")
    env.setdefault("PYTHONIOENCODING", "utf-8")
    env["PYTHONNOUSERSITE"] = "1"
    env.setdefault("GDAL_FILENAME_IS_UTF8", "YES")
    env.setdefault("SHAPE_ENCODING", "UTF-8")
    env.setdefault("CPL_ZIP_ENCODING", "UTF-8")
    return env


def run_subprocess(cmd, step_name):
    printable = " ".join(str(item) for item in cmd)
    print(f"{step_name}: {printable}", flush=True)
    restore_dll_dir = None
    run_cwd = None
    if os.name == "nt":
        try:
            import ctypes

            restore_dll_dir = getattr(sys, "_MEIPASS", None)
            set_dll_directory = ctypes.windll.kernel32.SetDllDirectoryW
            set_dll_directory.argtypes = [ctypes.c_wchar_p]
            set_dll_directory.restype = ctypes.c_int
            set_dll_directory(None)
        except (AttributeError, OSError, TypeError):
            restore_dll_dir = None
        executable = Path(str(cmd[0]))
        if executable.is_absolute() and executable.parent.is_dir():
            run_cwd = str(executable.parent)
    try:
        subprocess.run(cmd, check=True, env=gdal_subprocess_env(), cwd=run_cwd)
    except FileNotFoundError as exc:
        raise RuntimeError(
            f"{step_name} failed because a GDAL command was not found. "
            "Install GDAL command line tools, including gdal_polygonize.py and ogr2ogr."
        ) from exc
    except subprocess.CalledProcessError as exc:
        raise RuntimeError(f"{step_name} failed with exit code {exc.returncode}") from exc
    finally:
        if os.name == "nt" and restore_dll_dir:
            try:
                import ctypes

                set_dll_directory = ctypes.windll.kernel32.SetDllDirectoryW
                set_dll_directory.argtypes = [ctypes.c_wchar_p]
                set_dll_directory.restype = ctypes.c_int
                set_dll_directory(str(restore_dll_dir))
            except (AttributeError, OSError, TypeError):
                pass


def infer_vector_format(out_vector):
    suffix = Path(out_vector).suffix.lower()
    if suffix == ".shp":
        return "ESRI Shapefile"
    if suffix == ".gpkg":
        return "GPKG"
    return "ESRI Shapefile"


def prepare_vector_output_path(out_vector):
    out_path = Path(out_vector)
    if out_path.suffix:
        out_path.parent.mkdir(parents=True, exist_ok=True)
    else:
        out_path.mkdir(parents=True, exist_ok=True)
    return out_path



def geometry_without_holes(geom, ogr):
    if geom is None or geom.IsEmpty():
        return None
    geom_type = ogr.GT_Flatten(geom.GetGeometryType())
    if geom_type == ogr.wkbPolygon:
        if geom.GetGeometryCount() == 0:
            return None
        exterior = geom.GetGeometryRef(0)
        if exterior is None or exterior.GetPointCount() < 4:
            return None
        polygon = ogr.Geometry(ogr.wkbPolygon)
        polygon.AddGeometry(exterior.Clone())
        return polygon
    if geom_type == ogr.wkbMultiPolygon:
        multi = ogr.Geometry(ogr.wkbMultiPolygon)
        for idx in range(geom.GetGeometryCount()):
            polygon = geometry_without_holes(geom.GetGeometryRef(idx), ogr)
            if polygon is not None and not polygon.IsEmpty():
                multi.AddGeometry(polygon)
        return multi if multi.GetGeometryCount() > 0 else None
    if geom_type == ogr.wkbGeometryCollection:
        multi = ogr.Geometry(ogr.wkbMultiPolygon)
        for idx in range(geom.GetGeometryCount()):
            child = geometry_without_holes(geom.GetGeometryRef(idx), ogr)
            if child is None or child.IsEmpty():
                continue
            child_type = ogr.GT_Flatten(child.GetGeometryType())
            if child_type == ogr.wkbPolygon:
                multi.AddGeometry(child)
            elif child_type == ogr.wkbMultiPolygon:
                for sub_idx in range(child.GetGeometryCount()):
                    multi.AddGeometry(child.GetGeometryRef(sub_idx).Clone())
        return multi if multi.GetGeometryCount() > 0 else None
    return None


def fill_change_polygon_holes(src_gpkg, dst_gpkg, src_layer_name, dst_layer_name, value):
    try:
        from osgeo import ogr
    except ModuleNotFoundError as exc:
        raise RuntimeError("Python GDAL bindings are required for --fill_holes: from osgeo import ogr failed.") from exc

    src_ds = ogr.Open(str(src_gpkg), 0)
    if src_ds is None:
        raise RuntimeError(f"Could not open polygonized GPKG: {src_gpkg}")
    src_layer = src_ds.GetLayerByName(src_layer_name)
    if src_layer is None:
        raise RuntimeError(f"Could not find layer {src_layer_name} in {src_gpkg}")

    driver = ogr.GetDriverByName("GPKG")
    if driver is None:
        raise RuntimeError("OGR GPKG driver is not available.")
    if Path(dst_gpkg).exists():
        driver.DeleteDataSource(str(dst_gpkg))
    dst_ds = driver.CreateDataSource(str(dst_gpkg))
    if dst_ds is None:
        raise RuntimeError(f"Could not create filled GPKG: {dst_gpkg}")
    dst_layer = dst_ds.CreateLayer(dst_layer_name, src_layer.GetSpatialRef(), geom_type=ogr.wkbMultiPolygon)
    dst_layer.CreateField(ogr.FieldDefn("value", ogr.OFTInteger))
    dst_defn = dst_layer.GetLayerDefn()

    kept = 0
    for feature in src_layer:
        try:
            feature_value = int(feature.GetField("value"))
        except (TypeError, ValueError):
            continue
        if feature_value != int(value):
            continue
        cleaned = geometry_without_holes(feature.GetGeometryRef(), ogr)
        if cleaned is None or cleaned.IsEmpty():
            continue
        out_feature = ogr.Feature(dst_defn)
        out_feature.SetField("value", feature_value)
        out_feature.SetGeometry(cleaned)
        if dst_layer.CreateFeature(out_feature) != 0:
            raise RuntimeError("Failed to write a filled polygon feature.")
        kept += 1
        out_feature = None

    src_ds = None
    dst_ds = None
    print(f"Filled polygon holes: kept {kept} change polygons", flush=True)
    return kept

def vectorize_mask(mask_path, out_vector, work_dir, value=255, min_area=0.0, simplify=0.0, layer_name="change", fill_holes=False):
    mask_path = Path(mask_path)
    if not mask_path.exists():
        raise FileNotFoundError(f"Mask file does not exist: {mask_path}")
    out_path = prepare_vector_output_path(out_vector)
    raw_gpkg = Path(work_dir) / "polygonized_raw.gpkg"
    raw_layer = "raw"
    if raw_gpkg.exists():
        raw_gpkg.unlink()

    polygonize_cmd = gdal_polygonize_command() + [
        str(mask_path),
        "-b",
        "1",
        "-f",
        "GPKG",
        str(raw_gpkg),
        raw_layer,
        "value",
    ]
    run_subprocess(polygonize_cmd, "Polygonize mask")

    source_gpkg = raw_gpkg
    source_layer = raw_layer
    if fill_holes:
        filled_gpkg = Path(work_dir) / "polygonized_filled.gpkg"
        filled_layer = "filled"
        fill_change_polygon_holes(raw_gpkg, filled_gpkg, raw_layer, filled_layer, value)
        source_gpkg = filled_gpkg
        source_layer = filled_layer

    ogr2ogr = gdal_command("ogr2ogr")
    if not ogr2ogr or ogr2ogr == "ogr2ogr":
        raise RuntimeError("ogr2ogr was not found. Install GDAL command line tools before vector export.")

    where_parts = [f"value = {int(value)}"]
    if min_area and min_area > 0:
        where_parts.append(f"ST_Area(geom) >= {float(min_area)}")
    where_sql = " AND ".join(where_parts)
    geometry_expr = "geom"
    if simplify and simplify > 0:
        geometry_expr = f"ST_Simplify(geom, {float(simplify)})"
    sql = (
        f"SELECT value, ST_Area(geom) AS area_m2, {geometry_expr} AS geom "
        f"FROM {source_layer} WHERE {where_sql}"
    )

    vector_format = infer_vector_format(out_path)
    ogr_cmd = [
        ogr2ogr,
        "-overwrite",
        "-f",
        vector_format,
        str(out_path),
        str(source_gpkg),
        "-nln",
        layer_name,
        "-dialect",
        "SQLite",
        "-sql",
        sql,
    ]
    run_subprocess(ogr_cmd, "Export vector")
    print(f"Saved vector: {out_path}", flush=True)


def configure_deployment_paths(args):
    args._auto_work_dir = False
    if not Path(args.onnx_model).expanduser().is_file():
        raise FileNotFoundError(
            f"ONNX model does not exist: {args.onnx_model}. "
            "Pass --onnx_model or set the CHANGE_MODEL_PATH environment variable."
        )
    if args.output_dir:
        output_dir = Path(args.output_dir).expanduser()
        output_dir.mkdir(parents=True, exist_ok=True)
        prefix = Path(args.output_prefix).name
        if prefix != args.output_prefix or not prefix:
            raise ValueError("--output_prefix must be a plain non-empty filename prefix.")
        if args.out_prob is None:
            args.out_prob = str(output_dir / f"{prefix}_prob.tif")
        if args.out_mask is None:
            args.out_mask = str(output_dir / f"{prefix}_mask.tif")
        if args.out_vector is None and not args.no_vector:
            args.out_vector = str(output_dir / f"{prefix}.shp")
        if args.work_dir is None:
            safe_prefix = "".join(ch if ch.isascii() and (ch.isalnum() or ch in "-_") else "_" for ch in prefix)
            if not safe_prefix.strip("_"):
                safe_prefix = "change_result"
            unique = uuid.uuid4().hex[:8]
            args.work_dir = str(Path(tempfile.gettempdir()) / "ChangeDetectionApp" / f"{safe_prefix}_{unique}")
            args._auto_work_dir = True
            print(f"Auto work_dir: {args.work_dir}", flush=True)
    if args.out_prob is None:
        raise ValueError("Pass --output_dir or provide --out_prob explicitly.")


def ensure_mask_for_vector(args):
    if args.out_vector and not args.out_mask:
        out_prob = Path(args.out_prob)
        args.out_mask = str(out_prob.with_name(out_prob.stem + "_mask.tif"))
        print(f"--out_vector enabled; auto mask path: {args.out_mask}", flush=True)

def main():
    total_start = time.perf_counter()
    wall_start = time.strftime("%Y-%m-%d %H:%M:%S")
    print(f"Task started at: {wall_start}", flush=True)
    args = parse_args()
    configure_deployment_paths(args)
    ensure_mask_for_vector(args)
    tmp_owner = None
    task_success = False
    if args.work_dir:
        work_dir = Path(args.work_dir)
        work_dir.mkdir(parents=True, exist_ok=True)
    else:
        tmp_owner = tempfile.TemporaryDirectory(prefix="virtual_patch_vrt_")
        work_dir = Path(tmp_owner.name)
    try:
        prepare_start = time.perf_counter()
        phase_a = prepare_raster_path(args.phase_a, "phase_a", work_dir)
        phase_b = prepare_raster_path(args.phase_b, "phase_b", work_dir)
        print(f"Prepare time: {format_duration(time.perf_counter() - prepare_start)}", flush=True)
        args.phase_a = phase_a
        args.phase_b = phase_b
        predict_start = time.perf_counter()
        run_multi_gpu(args, phase_a, phase_b)
        print(f"Prediction time: {format_duration(time.perf_counter() - predict_start)}", flush=True)
        if not args.skip_prob:
            print(f"Saved probability: {args.out_prob}", flush=True)
        if args.out_mask:
            print(f"Saved mask: {args.out_mask}", flush=True)
        if args.out_vector:
            vector_start = time.perf_counter()
            vectorize_mask(
                args.out_mask,
                args.out_vector,
                work_dir,
                value=args.vector_value,
                min_area=args.vector_min_area,
                simplify=args.vector_simplify,
                layer_name=args.vector_layer,
                fill_holes=args.fill_holes,
            )
            print(f"Vectorization time: {format_duration(time.perf_counter() - vector_start)}", flush=True)
        total_elapsed = time.perf_counter() - total_start
        wall_end = time.strftime("%Y-%m-%d %H:%M:%S")
        print(f"Task finished at: {wall_end}", flush=True)
        print(f"Total task time: {format_duration(total_elapsed)} ({total_elapsed:.2f}s)", flush=True)
        task_success = True
    finally:
        if tmp_owner is not None:
            tmp_owner.cleanup()
        elif getattr(args, "_auto_work_dir", False):
            if task_success and not args.keep_parts:
                shutil.rmtree(work_dir, ignore_errors=True)
                print(f"Cleaned temporary work_dir: {work_dir}", flush=True)
            elif not task_success:
                print(f"Task failed; kept temporary work_dir for debugging: {work_dir}", flush=True)


if __name__ == "__main__":
    main()




