"""Read exported files back to verify geometry, attributes and interoperability."""
from copy import deepcopy
from io import BytesIO
import json
import tempfile
import unittest
from zipfile import ZipFile

from fastapi import HTTPException
from rasterio.crs import CRS
import shapefile
from shapely.geometry import MultiPolygon, Polygon, mapping, shape

from app.postprocess.shapefile_export import geojson_to_shp_zip
from app.routers.export import export_shp


class ShapefileExportTests(unittest.TestCase):
    def collection(self):
        polygon = Polygon([(114, 38), (116, 38), (116, 40), (114, 40)],
                          [[(114.5, 38.5), (115, 38.5), (115, 39), (114.5, 39)]])
        multi = MultiPolygon([Polygon([(117, 38), (118, 38), (118, 39), (117, 39)]),
                              Polygon([(119, 38), (120, 38), (120, 39), (119, 39)])])
        return {"type": "FeatureCollection", "crs": {"type": "name", "properties": {"name": "EPSG:4326"}},
                "features": [
                    {"type": "Feature", "id": "patch-1", "geometry": mapping(polygon), "properties": {
                        "patch_id": "图斑一", "value": 1, "change_type": "未分类变化", "area_pixels": 35,
                        "area_m2": 123.456, "area_ha": 0.012346, "perimeter_m": 45.123,
                        "perimeter_crs": "EPSG:32650", "centroid": [115, 39], "confidence_mean": 0.876543,
                        "confidence_valid_pixels": 34, "extra": {"中文": "保留"}}},
                    {"type": "Feature", "id": "patch-2", "geometry": mapping(multi),
                     "properties": {"confidence_mean": None}}]}

    def test_archive_readback(self):
        collection = self.collection()
        with tempfile.TemporaryDirectory() as directory:
            with ZipFile(BytesIO(geojson_to_shp_zip(collection))) as archive:
                self.assertEqual(set(archive.namelist()), {"change.shp", "change.shx", "change.dbf",
                                 "change.prj", "change.cpg", "README.txt", "change.geojson"})
                self.assertEqual(archive.read("change.cpg"), b"UTF-8")
                self.assertEqual(CRS.from_wkt(archive.read("change.prj").decode()).to_epsg(), 4326)
                self.assertEqual(json.loads(archive.read("change.geojson")), json.loads(json.dumps(collection)))
                self.assertIn("conf_mean: confidence_mean", archive.read("README.txt").decode())
                archive.extractall(directory)
            with shapefile.Reader(f"{directory}/change.shp", encoding="utf-8") as reader:
                self.assertEqual(reader.shapeType, shapefile.POLYGON)
                self.assertEqual(len(reader), 2)
                self.assertTrue(all(len(field[0]) <= 10 for field in reader.fields[1:]))
                for feature, exported in zip(collection["features"], reader.shapes()):
                    self.assertTrue(shape(feature["geometry"]).equals(shape(exported.__geo_interface__)))
                # Exterior clockwise, hole counterclockwise, multipart stays one record.
                rings = reader.shape(0).parts
                points = reader.shape(0).points
                self.assertLess(shapefile.signed_area(points[rings[0]:rings[1]]), 0)
                self.assertGreater(shapefile.signed_area(points[rings[1]:]), 0)
                first, second = [record.as_dict() for record in reader.records()]
                self.assertEqual(first["patch_id"], "图斑一")
                self.assertEqual(first["chg_type"], "未分类变化")
                self.assertEqual(first["area_px"], 35)
                self.assertEqual(first["area_m2"], 123.456)
                self.assertEqual(first["conf_mean"], 0.876543)
                self.assertEqual([first["ctr_lon"], first["ctr_lat"]], [115, 39])
                self.assertEqual(second["patch_id"], "patch-2")
                self.assertIsNone(second["conf_mean"])
                self.assertIsNone(second["area_m2"])

    def test_empty_layer(self):
        with ZipFile(BytesIO(geojson_to_shp_zip({"type": "FeatureCollection", "features": []}))) as archive:
            with shapefile.Reader(**{suffix: BytesIO(archive.read(f"change.{suffix}"))
                                     for suffix in ("shp", "shx", "dbf")}) as reader:
                self.assertEqual(reader.shapeType, shapefile.POLYGON)
                self.assertEqual(len(reader), 0)
                self.assertGreater(len(reader.fields), 1)

    def test_invalid_input_and_route_errors(self):
        invalid = [{}, {"type": "FeatureCollection", "features": None}]
        for geometry in (None, {"type": "Point", "coordinates": [114, 38]},
                         {"type": "Polygon", "coordinates": []},
                         {"type": "Polygon", "coordinates": [[[114, 38], [115, 39], [114, 39], [115, 38], [114, 38]]]}):
            collection = self.collection()
            collection["features"][0]["geometry"] = geometry
            invalid.append(collection)
        for key, value in (("confidence_mean", float("nan")), ("area_pixels", 1.5),
                           ("change_type", "中" * 100), ("centroid", [114]), ("area_m2", 1e30)):
            collection = self.collection()
            collection["features"][0]["properties"][key] = value
            invalid.append(collection)
        collection = deepcopy(self.collection())
        collection["crs"]["properties"]["name"] = "EPSG:3857"
        invalid.append(collection)
        for collection in invalid:
            with self.subTest(collection=collection):
                with self.assertRaises(ValueError):
                    geojson_to_shp_zip(collection)
                with self.assertRaises(HTTPException) as error:
                    export_shp(collection)
                self.assertEqual(error.exception.status_code, 400)

    def test_route_returns_zip(self):
        response = export_shp(self.collection())
        self.assertEqual(response.media_type, "application/zip")
        with ZipFile(BytesIO(response.body)) as archive:
            self.assertIn("change.shp", archive.namelist())


if __name__ == "__main__":
    unittest.main()
