"""Convert existing task geometry without re-running detection."""
import logging

from fastapi import APIRouter, Body, HTTPException, Response

from app.postprocess.shapefile_export import geojson_to_shp_zip

router = APIRouter(prefix="/py/export", tags=["export"])
logger = logging.getLogger(__name__)


@router.post("/shp")
def export_shp(collection: dict = Body(...)):
    try:
        archive = geojson_to_shp_zip(collection)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    except Exception as exc:
        logger.exception("SHP export failed")
        raise HTTPException(status_code=500, detail="SHP 转换失败") from exc
    return Response(archive, media_type="application/zip")
