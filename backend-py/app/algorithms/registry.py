"""算法注册表：@register 装饰器 + get/get_all 查询。"""
from __future__ import annotations

from typing import Dict, List

from app.algorithms.base import ChangeDetectionAlgorithm

_REGISTRY: Dict[str, ChangeDetectionAlgorithm] = {}


def register(cls: type) -> type:
    """将算法类实例化并按 name 注册到全局表。"""
    instance = cls()
    if not instance.name:
        raise ValueError(f"{cls.__name__}.name 不能为空")
    if instance.name in _REGISTRY:
        raise ValueError(f"算法名重复: {instance.name}")
    _REGISTRY[instance.name] = instance
    return cls


def get(name: str) -> ChangeDetectionAlgorithm:
    """按 name 取算法实例；不存在抛 KeyError。"""
    if name not in _REGISTRY:
        raise KeyError(name)
    return _REGISTRY[name]


def get_all() -> List[ChangeDetectionAlgorithm]:
    """返回所有已注册算法实例列表。"""
    return list(_REGISTRY.values())
