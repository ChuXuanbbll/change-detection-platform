"""Normalize the bundled DataV/AMap GCJ-02 boundary to WGS84, offline.
Input and output paths are relative to the project, not the import machine.
"""
import hashlib
import json
import math
from pathlib import Path
from shapely import make_valid
from shapely.geometry import shape, mapping, MultiPolygon
from shapely.ops import transform, unary_union

ROOT = Path(__file__).resolve().parents[1] / 'data/boundaries'
SOURCE_URL = 'https://geo.datav.aliyun.com/areas_v3/bound/130000.json'

def forward(lng, lat):
    x, y = lng - 105, lat - 35
    pi = math.pi
    dlat = -100 + 2*x + 3*y + .2*y*y + .1*x*y + .2*math.sqrt(abs(x))
    dlat += (20*math.sin(6*x*pi)+20*math.sin(2*x*pi))*2/3
    dlat += (20*math.sin(y*pi)+40*math.sin(y*pi/3))*2/3
    dlat += (160*math.sin(y*pi/12)+320*math.sin(y*pi/30))*2/3
    dlng = 300+x+2*y+.1*x*x+.1*x*y+.1*math.sqrt(abs(x))
    dlng += (20*math.sin(6*x*pi)+20*math.sin(2*x*pi))*2/3
    dlng += (20*math.sin(x*pi)+40*math.sin(x*pi/3))*2/3
    dlng += (150*math.sin(x*pi/12)+300*math.sin(x*pi/30))*2/3
    rad = lat*pi/180
    magic = 1-.00669342162296594323*math.sin(rad)**2
    a=6378137.0
    dlat = dlat*180/((a*(1-.00669342162296594323)/(magic*math.sqrt(magic)))*pi)
    dlng = dlng*180/((a/math.sqrt(magic))*math.cos(rad)*pi)
    return lng+dlng,lat+dlat

def inverse(lng, lat, z=None):
    x,y=lng,lat
    for _ in range(10):
        gx,gy=forward(x,y)
        x+=lng-gx; y+=lat-gy
    return x,y

def polygons(geom):
    if geom.geom_type == 'Polygon': return [geom]
    return [p for g in getattr(geom,'geoms',[]) for p in polygons(g)]

def main():
    raw=(ROOT/'hebei-source.geojson').read_bytes()
    data=json.loads(raw)
    features=[f for f in data['features'] if f['properties'].get('adcode')==130000]
    if len(features)!=1: raise ValueError('Expected exactly one Hebei province feature')
    original=shape(features[0]['geometry'])
    projected=transform(inverse,original)
    boundary=unary_union(polygons(make_valid(projected)))
    if not boundary.is_valid or boundary.is_empty: raise ValueError('Invalid normalized province')
    if boundary.geom_type=='Polygon': boundary=MultiPolygon([boundary])
    provenance=dict(source_url=SOURCE_URL,source_sha256=hashlib.sha256(raw).hexdigest(),
        source_coordinate_system='GCJ-02 (DataV/AMap map coordinates)',coordinate_system='EPSG:4326',
        normalization='10-step inverse GCJ-02 approximation; Shapely make_valid; polygon components only',
        source_geometry_valid=original.is_valid, polygon_count=len(boundary.geoms),
        note='Public visualization boundary, not a cadastral or survey-grade boundary.')
    output={'type':'FeatureCollection','features':[{'type':'Feature','properties':{'adcode':130000,'name':'河北省',**provenance},'geometry':mapping(boundary)}]}
    (ROOT/'hebei.geojson').write_text(json.dumps(output,ensure_ascii=False,separators=(',',':')),encoding='utf-8')
    (ROOT/'provenance.json').write_text(json.dumps(provenance,ensure_ascii=False,indent=2),encoding='utf-8')
    print('Boundary ready:',boundary.bounds,'polygons:',len(boundary.geoms))

if __name__=='__main__': main()
