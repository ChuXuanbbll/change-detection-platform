"""In-memory WGS84 shapefile export with stable DBF field names."""
from io import BytesIO
import json
import math
from zipfile import ZIP_DEFLATED, ZipFile

import shapefile
from rasterio.crs import CRS
from shapely.geometry import shape
from shapely.geometry.polygon import orient

# source property, DBF name, type, width, decimal places
FIELDS = [
    ("patch_id", "patch_id", "C", 254, 0),
    ("value", "value", "N", 20, 0),
    ("change_type", "chg_type", "C", 254, 0),
    ("area_pixels", "area_px", "N", 20, 0),
    ("area_m2", "area_m2", "N", 20, 3),
    ("area_ha", "area_ha", "N", 20, 6),
    ("perimeter_m", "perim_m", "N", 20, 3),
    ("perimeter_crs", "perim_crs", "C", 64, 0),
    ("centroid_lon", "ctr_lon", "N", 20, 10),
    ("centroid_lat", "ctr_lat", "N", 20, 10),
    ("confidence_mean", "conf_mean", "N", 20, 6),
    ("confidence_valid_pixels", "conf_px", "N", 20, 0),
]


def _dbf_value(value, name, kind, width, decimals):
    if value is None:
        return None
    if kind == "C":
        if not isinstance(value, str) or len(value.encode("utf-8")) > width:
            raise ValueError(f"属性 {name} 必须为不超过 {width} 字节的字符串")
    else:
        if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
            raise ValueError(f"属性 {name} 必须为有限数值")
        if decimals == 0 and value != int(value):
            raise ValueError(f"属性 {name} 必须为整数")
        text = str(int(value)) if decimals == 0 else f"{value:.{decimals}f}"
        if len(text) > width:
            raise ValueError(f"属性 {name} 超出 DBF 数值范围")
    return value


def _polygon_parts(geometry):
    if not isinstance(geometry, dict) or geometry.get("type") not in ("Polygon", "MultiPolygon"):
        raise ValueError("SHP 导出仅支持 Polygon 和 MultiPolygon 图斑")
    try:
        polygon = shape(geometry)
    except Exception as exc:
        raise ValueError("图斑坐标无效") from exc
    if polygon.is_empty or not polygon.is_valid:
        raise ValueError("图斑几何为空或无效")
    polygons = [polygon] if polygon.geom_type == "Polygon" else list(polygon.geoms)
    parts = []
    for item in polygons:
        # Shapefile exteriors are clockwise; holes are counterclockwise.
        item = orient(item, sign=-1.0)
        for ring in [item.exterior, *item.interiors]:
            points = [list(point) for point in ring.coords]
            if any(len(p) != 2 or not all(math.isfinite(n) for n in p)
                   or not (-180 <= p[0] <= 180 and -90 <= p[1] <= 90) for p in points):
                raise ValueError("图斑必须使用有效的 WGS84 二维经纬度坐标")
            parts.append(points)
    return parts


def geojson_to_shp_zip(collection: dict) -> bytes:
    if collection.get("type") != "FeatureCollection" or not isinstance(collection.get("features"), list):
        raise ValueError("GeoJSON 必须为有效的 FeatureCollection")
    crs = collection.get("crs")
    if crs is not None:
        try:
            name = crs["properties"]["name"]
            is_wgs84 = name in ("urn:ogc:def:crs:OGC:1.3:CRS84", "OGC:CRS84") or CRS.from_string(name) == CRS.from_epsg(4326)
        except Exception as exc:
            raise ValueError("GeoJSON 坐标系无效") from exc
        if not is_wgs84:
            raise ValueError("SHP 导出要求 GeoJSON 使用 WGS84 坐标系")
    streams = {suffix: BytesIO() for suffix in ("shp", "shx", "dbf")}
    with shapefile.Writer(**streams, shapeType=shapefile.POLYGON, encoding="utf-8") as writer:
        for _, name, kind, width, decimals in FIELDS:
            writer.field(name, kind, size=width, decimal=decimals)
        for index, feature in enumerate(collection["features"]):
            if not isinstance(feature, dict) or feature.get("type") != "Feature":
                raise ValueError(f"第 {index + 1} 条图斑不是有效 Feature")
            parts = _polygon_parts(feature.get("geometry"))
            properties = feature.get("properties")
            if properties is None:
                properties = {}
            if not isinstance(properties, dict):
                raise ValueError("图斑 properties 必须为对象或 null")
            values = dict(properties)
            if values.get("patch_id") is None and feature.get("id") is not None:
                values["patch_id"] = str(feature["id"])
            centroid = values.get("centroid")
            if centroid is not None:
                if not isinstance(centroid, (list, tuple)) or len(centroid) != 2:
                    raise ValueError("图斑 centroid 必须为经纬度两元素数组")
                values["centroid_lon"], values["centroid_lat"] = centroid
            record = [_dbf_value(values.get(source), name, kind, width, decimals)
                      for source, name, kind, width, decimals in FIELDS]
            writer.poly(parts)
            writer.record(*record)
    archive = BytesIO()
    with ZipFile(archive, "w", compression=ZIP_DEFLATED) as output:
        for suffix, stream in streams.items():
            output.writestr(f"change.{suffix}", stream.getvalue())
        output.writestr("change.prj", CRS.from_epsg(4326).to_wkt(version="WKT1_ESRI"))
        output.writestr("change.cpg", "UTF-8")
        descriptions = "\n".join(f"{name}: {source}" for source, name, *_ in FIELDS)
        output.writestr("README.txt", "坐标系: WGS84 / EPSG:4326；坐标顺序: 经度、纬度。\n"
                        "编码: UTF-8。每条记录对应一个变化图斑。\n"
                        "面积单位: m² / ha；周长单位: m；置信度为变化分数。\n"
                        "缺失数值为空；缺失文本为空字符串。DBF 字段映射:\n" + descriptions)
        # Preserve feature IDs, extra properties and collection metadata too.
        output.writestr("change.geojson", json.dumps(collection, ensure_ascii=False, allow_nan=False))
    return archive.getvalue()
