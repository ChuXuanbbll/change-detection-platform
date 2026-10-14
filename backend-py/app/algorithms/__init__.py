"""Algorithms package: 导入以触发 @register 装饰器注册。"""
from app.algorithms.registry import get, get_all, register

from app.algorithms import hebei_onnx  # noqa: F401

__all__ = ["get", "get_all", "register"]
