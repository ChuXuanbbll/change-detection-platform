"""算法查询与健康检查路由。"""
from fastapi import APIRouter

from app.algorithms import get_all

router = APIRouter(prefix="/py", tags=["algorithms"])


@router.get("/health")
def health() -> dict:
    """健康检查：列出已注册算法名。"""
    algos = get_all()
    return {"status": "ok", "algorithms": [a.name for a in algos]}


@router.get("/algorithms")
def list_algorithms() -> list:
    """列出所有已注册算法元数据。"""
    out = []
    for algo in get_all():
        out.append(
            {
                "name": algo.name,
                "display_name": algo.display_name,
                "params_schema": algo.params_schema,
                "input_bands": algo.input_bands,
            }
        )
    return out
