"""Use the bundled real boundary, not a rectangular mock."""
import hashlib
import io
import json
import math
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import numpy as np
import rasterio
from PIL import Image
from rasterio.features import geometry_mask
from shapely.geometry import Point, box, mapping
from app.fetchers import local
from app.routers.wayback import list_catalog, tile
from fastapi import HTTPException

class HebeiBoundaryTests(unittest.TestCase):
    def setUp(self):
        self.imagery = patch.object(local, 'IMAGERY', local.DATA / 'imagery')
        self.imagery.start()
        local.catalog.cache_clear()
        self.addCleanup(self.imagery.stop)
        self.addCleanup(local.catalog.cache_clear)

    def tearDown(self):
        local.province.cache_clear()
        local.province_mercator.cache_clear()
        local.render_tile.cache_clear()

    def test_boundary_provenance_and_validity(self):
        p=local.province()
        self.assertTrue(p.is_valid)
        self.assertEqual(p.geom_type,'MultiPolygon')
        metadata=json.loads((local.BOUNDARY.parent/'provenance.json').read_text(encoding='utf-8'))
        self.assertEqual(hashlib.sha256((local.BOUNDARY.parent/'hebei-source.geojson').read_bytes()).hexdigest(),metadata['source_sha256'])
        self.assertEqual(metadata['coordinate_system'],'EPSG:4326')

    def test_cities_and_hebei_exclave(self):
        p=local.province()
        for point in [(114.32,38.11),(116.26,39.44),(117.07,39.98)]:
            self.assertTrue(p.covers(Point(*point)),point)
        for point in [(116.4,39.9),(117.2,39.12),(113.65,34.75)]:
            self.assertFalse(p.covers(Point(*point)),point)
            lng,lat=point
            with self.assertRaises(ValueError):local.selection([lng-.001,lat-.001,lng+.001,lat+.001])

    def test_missing_boundary_fails_closed(self):
        with tempfile.TemporaryDirectory() as temp:
            with patch.object(local,'BOUNDARY',Path(temp)/'missing.geojson'):
                local.province.cache_clear()
                with self.assertRaises(local.BoundaryUnavailable):local.selection([114.3,38.1,114.31,38.11])
                for call in (list_catalog,lambda:tile('latest',7,0,0)):
                    with self.assertRaises(HTTPException) as exc:call()
                    self.assertEqual(exc.exception.status_code,503)

    def test_real_guan_border_crop_and_tile(self):
        region=local.province()
        scene=next(r for r in local.catalog() if r['id']=='scene-202604-guan')
        border=region.boundary.intersection(box(*scene['bounds']))
        point=border.interpolate(.5,normalized=True)
        bounds=[point.x-.002,point.y-.002,point.x+.002,point.y+.002]
        with tempfile.TemporaryDirectory() as temp:
            result=local.crop_images(bounds,'2026-04-01','2026-04-30',1,temp)
            self.assertEqual(result['count'],1)
            with rasterio.open(result['images'][0]['path']) as src:
                alpha=src.dataset_mask()
                inside=geometry_mask([mapping(region)],src.shape,src.transform,invert=True)
                self.assertTrue((alpha>0).any())
                self.assertTrue((~inside).any())
                self.assertFalse((alpha[~inside]>0).any())
        z=15; x=int((point.x+180)/360*2**z)
        y=int((1-math.asinh(math.tan(math.radians(point.y)))/math.pi)/2*2**z)
        rgba=np.array(Image.open(io.BytesIO(local.render_tile(scene['id'],z,x,y))))
        extent=20037508.342789244;step=2*extent/2**z
        transform=rasterio.transform.from_bounds(-extent+x*step,extent-(y+1)*step,-extent+(x+1)*step,extent-y*step,256,256)
        inside=geometry_mask([local.province_mercator()],(256,256),transform,invert=True)
        self.assertTrue((rgba[:,:,3]>0).any())
        self.assertTrue((~inside).any())
        self.assertFalse((rgba[:,:,3][~inside]>0).any())

if __name__=='__main__':unittest.main()
