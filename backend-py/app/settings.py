"""Validated runtime settings. Go owns persisted revisions; readers use immutable snapshots."""

from pathlib import Path
import os
from pydantic import BaseModel, ConfigDict, Field, model_validator

PROJECT = Path(__file__).resolve().parents[2]
DATA_ROOT = Path(os.getenv('DATA_DIR', str(PROJECT / 'backend-go/data'))).resolve()
ASSET_ROOT = Path(os.getenv('ASSET_DIR', str(PROJECT / 'data'))).resolve()


class Strict(BaseModel):
    model_config = ConfigDict(extra='forbid')


class Source(Strict):
    id: str = Field(min_length=1, max_length=80, pattern=r'^[a-zA-Z0-9_-]+$')
    name: str = Field(min_length=1, max_length=120)
    path: str = Field(min_length=1)
    year: int | None = Field(None, ge=1900, le=2199)
    enabled: bool = True


class Detection(Strict):
    threshold: float = Field(0.5, ge=0, le=1, allow_inf_nan=False)
    resolution: float = Field(0.8, ge=0.01, allow_inf_nan=False)
    kernel_size: int = Field(3, ge=1, le=99)
    min_area_pixels: int = Field(10, ge=0)


class Runtime(Strict):
    gpu_device: int = Field(0, ge=0, le=31)
    batch_size: int = Field(1, ge=1, le=64)
    prefetch_batches: int = Field(2, ge=0, le=16)
    timeout_seconds: int = Field(3600, ge=30, le=86400)
    workers: int = Field(1, ge=1, le=8)
    queue_capacity: int = Field(128, ge=1, le=1024)


class Tiles(Strict):
    concurrency: int = Field(4, ge=1, le=8)
    max_pending: int = Field(8, ge=1, le=64)
    wait_ms: int = Field(1000, ge=5, le=1000)
    cache_items: int = Field(512, ge=0, le=8192)
    cache_mb: int = Field(64, ge=1, le=1024)
    handles: int = Field(8, ge=1, le=32)
    gdal_cache_mb: int = Field(128, ge=16, le=512)

    @model_validator(mode='after')
    def queue_limit(self):
        if self.max_pending < self.concurrency:
            raise ValueError('瓦片队列容量不能小于渲染并发')
        return self


class Browser(Strict):
    concurrency: int = Field(4, ge=1, le=16)
    inactive_layers: int = Field(3, ge=0, le=12)
    cache_mb: int = Field(128, ge=16, le=1024)
    idle_seconds: int = Field(300, ge=10, le=3600)


class Settings(Strict):
    sources: list[Source]
    default_model: str = 'builtin-hebei'
    detection: Detection = Field(default_factory=Detection)
    runtime: Runtime = Field(default_factory=Runtime)
    tiles: Tiles = Field(default_factory=Tiles)
    browser: Browser = Field(default_factory=Browser)

    @model_validator(mode='after')
    def unique_sources(self):
        if len({s.id for s in self.sources}) != len(self.sources):
            raise ValueError('影像来源 ID 重复')
        roots = []
        for s in self.sources:
            p = Path(s.path).expanduser()
            if not p.is_absolute():
                raise ValueError('影像来源必须为服务端绝对路径')
            p = p.resolve()
            if s.enabled:
                if any(p == r or p.is_relative_to(r) or r.is_relative_to(p) for r in roots):
                    raise ValueError('影像来源目录重复或重叠')
                roots.append(p)
        return self


active_revision = 0
active_settings: Settings | None = None


def defaults():
    source = Source(
        id='local-default',
        name='现有影像',
        path=str(Path(os.getenv('LOCAL_IMAGERY_DIR', 'Y:/河北影像/2025年')).resolve()),
    )
    tiles = Tiles(
        concurrency=int(os.getenv('TILE_RENDER_CONCURRENCY', 4)),
        max_pending=int(
            os.getenv('TILE_MAX_PENDING', int(os.getenv('TILE_RENDER_CONCURRENCY', 4)) * 2)
        ),
        wait_ms=int(os.getenv('TILE_QUEUE_WAIT_MS', 1000)),
        handles=int(os.getenv('TILE_DATASET_HANDLES', 8)),
        gdal_cache_mb=int(os.getenv('TILE_GDAL_CACHE_MB', 128)),
    )
    return Settings(sources=[source], tiles=tiles)


def managed_path(value, *, output=False):
    p = Path(value).expanduser().resolve()
    if not p.is_relative_to(DATA_ROOT):
        raise ValueError('文件路径必须位于运行数据目录内')
    if output and p == DATA_ROOT:
        raise ValueError('输出不能覆盖数据根目录')
    return p
