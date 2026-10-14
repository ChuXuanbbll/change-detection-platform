"""Private configuration control plane, authenticated by a local shared token."""

import asyncio
import hashlib
import json
import os
from pathlib import Path
import anyio
from fastapi import APIRouter, HTTPException, Request
from pydantic import BaseModel, Field
from app import settings
from app.fetchers import local
from app.utils.tile_queue import TileRenderer
from app.utils import model_process

router = APIRouter(prefix='/py/admin', tags=['internal'])
_apply_lock = asyncio.Lock()
_last_probe = None


def model_path():
    return Path(
        os.getenv(
            'CHANGE_MODEL_PATH', str(settings.PROJECT / 'model-runtime/model/enforcement_2.onnx')
        )
    ).resolve()


def pipeline_path():
    return settings.PROJECT / 'model-runtime/src/predict_city_virtual_patch_pipeline_onnx.py'


@router.get('/bootstrap')
def bootstrap():
    digest = ''
    try:
        with model_path().open('rb') as stream:
            digest = hashlib.file_digest(stream, 'sha256').hexdigest()
    except OSError:
        pass
    return {
        'settings': settings.defaults().model_dump(),
        'model_path': str(model_path()),
        'sha256': digest,
        'diagnostics': runtime_diagnostics(),
    }


@router.get('/status')
def diagnostics(request: Request):
    return runtime_diagnostics(getattr(request.app.state, 'tile_renderer', None))


def runtime_diagnostics(renderer=None):
    return {
        'revision': settings.active_revision,
        'python': model_process.interpreter(),
        'cuda_directory': model_process.cuda_directory(),
        'data_dir': str(settings.DATA_ROOT),
        'tile_runtime': {
            key: getattr(renderer, key, None)
            for key in ('concurrency', 'max_pending', 'cache_bytes', 'handles', 'gdal_cache_mb')
        },
        'asset_dir': str(settings.ASSET_ROOT),
        'required_provider': 'cuda',
        'cuda_verified': _last_probe,
    }


class Apply(BaseModel):
    revision: int
    settings: settings.Settings


@router.post('/validate')
def validate(config: settings.Settings):
    try:
        rows = local.build_source_catalog(config.sources)
    except (OSError, ValueError) as exc:
        raise HTTPException(400, str(exc)) from exc
    return {
        'settings': config.model_dump(),
        'scenes': len(rows),
        'available': sum(r.get('available', True) for r in rows),
    }


@router.post('/apply')
async def apply(req: Apply, request: Request):
    async with _apply_lock:
        if settings.active_revision == req.revision:
            return {'revision': req.revision}
        previous = settings.active_settings
        sources_changed = previous is None or previous.sources != req.settings.sources
        tiles_changed = previous is None or previous.tiles != req.settings.tiles
        try:
            rows = (
                await anyio.to_thread.run_sync(local.build_source_catalog, req.settings.sources)
                if sources_changed
                else local.catalog()
            )
        except (OSError, ValueError) as exc:
            raise HTTPException(400, str(exc)) from exc
        old = getattr(request.app.state, 'tile_renderer', None)
        renderer = old
        if tiles_changed or old is None:
            t = req.settings.tiles
            renderer = TileRenderer(
                concurrency=t.concurrency,
                max_pending=t.max_pending,
                wait_seconds=t.wait_ms / 1000,
                cache_items=t.cache_items,
                cache_bytes=t.cache_mb * 1024 * 1024,
                handles=t.handles,
                gdal_cache_mb=t.gdal_cache_mb,
            )
            await renderer.start()
        if await request.is_disconnected():
            if renderer is not old:
                await renderer.close()
            raise HTTPException(499, '配置请求已断开，取消发布')
        try:
            with local._catalog_lock:
                if sources_changed:
                    local._clear_catalog()
                    local._source_snapshot = rows
                settings.active_settings = req.settings.model_copy(deep=True)
                settings.active_revision = req.revision
                request.app.state.tile_renderer = renderer
        except BaseException:
            if renderer is not old:
                await renderer.close()
            raise
        if old is not None and old is not renderer:
            try:
                await old.close()
            except Exception:
                import logging

                logging.getLogger(__name__).exception('旧瓦片进程排空失败，新版本保持生效')
        return {'revision': req.revision, 'scenes': len(rows)}


class ModelProbe(BaseModel):
    path: str
    sha256: str = ''
    mapping: dict[str, str] = Field(default_factory=dict)
    builtin: bool = False
    gpu_device: int = 0


@router.post('/models/validate')
def validate_model(req: ModelProbe):
    try:
        return _validate_model(req)
    except HTTPException:
        raise
    except Exception as exc:
        raise HTTPException(400, f'模型验证失败: {type(exc).__name__}: {exc}') from exc


def _validate_model(req: ModelProbe):
    global _last_probe
    _last_probe = None
    import onnx
    from onnx.external_data_helper import _get_all_tensors

    path = Path(req.path).resolve()
    if path != model_path():
        settings.managed_path(path)
    with path.open('rb') as stream:
        digest = hashlib.file_digest(stream, 'sha256').hexdigest()
    if req.sha256 and req.sha256 != digest:
        raise HTTPException(400, '模型文件内容已变化')
    graph = onnx.load(str(path), load_external_data=False)
    if any(t.data_location == onnx.TensorProto.EXTERNAL for t in _get_all_tensors(graph)):
        raise HTTPException(400, '首版只支持内嵌权重的单文件 ONNX')
    onnx.checker.check_model(str(path))
    initializers = {t.name for t in graph.graph.initializer}

    def describe(value):
        t = value.type.tensor_type
        return {
            'name': value.name,
            'dtype': t.elem_type,
            'shape': [d.dim_value or d.dim_param or None for d in t.shape.dim],
        }

    inputs = [describe(i) for i in graph.graph.input if i.name not in initializers]
    outputs = [describe(i) for i in graph.graph.output]
    mapping = dict(req.mapping)
    if len(inputs) not in (1, 2) or any(
        len(i['shape']) != 4 or i['dtype'] not in (1, 10) for i in inputs
    ):
        raise HTTPException(400, '模型必须为一个六通道或两个三通道 NCHW 浮点输入')
    channels = 6 if len(inputs) == 1 else 3
    if any(isinstance(i['shape'][1], int) and i['shape'][1] != channels for i in inputs):
        raise HTTPException(400, '输入通道数不符合六通道或双三通道协议')
    if req.builtin or len(inputs) == 1:
        mapping.setdefault('input_a', inputs[0]['name'])
        if len(inputs) == 2:
            mapping.setdefault('input_b', inputs[1]['name'])
    if len(outputs) == 1:
        mapping.setdefault('output', outputs[0]['name'])
    report = dict(
        sha256=digest,
        inputs=inputs,
        outputs=outputs,
        mapping=mapping,
        status='needs_mapping',
        error='请指定前后时相输入和变化 logits 输出',
    )
    names = {i['name'] for i in inputs}
    if (
        mapping.get('input_a') not in names
        or (
            len(inputs) == 2
            and (
                mapping.get('input_b') not in names
                or mapping.get('input_a') == mapping.get('input_b')
            )
        )
        or mapping.get('output') not in {o['name'] for o in outputs}
    ):
        return report
    output = next(o for o in outputs if o['name'] == mapping['output'])
    if (
        len(output['shape']) != 4
        or output['dtype'] not in (1, 10)
        or (isinstance(output['shape'][1], int) and output['shape'][1] not in (1, 2))
    ):
        raise HTTPException(400, '输出必须为单通道或双通道 NCHW 浮点 logits')
    result = model_process.run(
        [
            model_process.interpreter(),
            str(Path(__file__).parents[1] / 'utils/probe_model.py'),
            str(pipeline_path()),
            str(path),
            json.dumps(mapping),
            str(req.gpu_device),
        ],
        timeout=180,
    )
    if result.returncode:
        _last_probe = {
            'available': False,
            'gpu_device': req.gpu_device,
            'error': (result.stderr or result.stdout)[-4000:],
        }
        report.update(status='invalid', error=(result.stderr or result.stdout)[-4000:])
    else:
        _last_probe = {
            **json.loads(result.stdout.strip().splitlines()[-1]),
            'gpu_device': req.gpu_device,
            'sha256': digest,
        }
        report.update(status='ready', error='', probe=_last_probe)
    return report


@router.post('/detection/validate')
def validate_detection(params: settings.Detection):
    return params.model_dump()


@router.post('/stop')
def stop_owned_models():
    model_process.stop_all()
    return {'stopped': True}
