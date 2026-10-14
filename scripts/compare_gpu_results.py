"""Compare preserved GPU task output to the pre-change detection pipeline."""
import ast
import json
from pathlib import Path
import sqlite3
import subprocess
import sys
import time

ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT/'backend-py'))

def main():
    import numpy as np
    import rasterio
    from app.routers import detect as current
    work=Path(sys.argv[1]).resolve()
    report=json.loads((work/'report.json').read_text(encoding='utf-8'))
    result=report['result'];task_id=result['task_id']
    with sqlite3.connect(f'{(work/"data/meta.db").as_uri()}?mode=ro',uri=True) as db:
        before,after,params=db.execute('SELECT b.file_path,a.file_path,t.params_json FROM tasks t JOIN images b ON b.id=t.before_id JOIN images a ON a.id=t.after_id WHERE t.id=?',(task_id,)).fetchone()
    original=subprocess.check_output(['git','show','17acd04:backend-py/app/routers/detect.py'],cwd=ROOT).decode('utf-8')
    functions=[node for node in ast.parse(original).body if isinstance(node,ast.FunctionDef)]
    for function in functions:function.decorator_list=[]
    namespace=dict(vars(current));namespace['ExternalModelError']=type('LegacyExternalModelError',(Exception,),{})
    exec(compile(ast.Module(body=functions,type_ignores=[]),'baseline-detect','exec'),namespace)
    started=time.perf_counter()
    baseline=namespace['detect'](current.DetectRequest(before_path=before,after_path=after,params=json.loads(params),output_dir=str(work/'baseline-result')))
    comparison={'baseline_seconds':time.perf_counter()-started,'reference_commit':'17acd04','model_hash':report['model_hash']}
    for key in ('mask_path','confidence_path'):
        with rasterio.open(baseline[key]) as a,rasterio.open(result[key]) as b:
            x,y=a.read(),b.read()
            assert a.transform==b.transform and a.crs==b.crs and np.array_equal(x,y),key
            comparison[key]='identical grid and pixels'
    assert json.loads(Path(baseline['geojson_path']).read_text(encoding='utf-8'))==result['geojson']
    actual=json.loads(result['stats_json'])
    for key,value in baseline['stats'].items():assert actual[key]==value,(key,actual[key],value)
    comparison.update(geojson='identical',statistics='identical',pixels=actual['total_pixels'])
    (work/'baseline-comparison.json').write_text(json.dumps(comparison,indent=2),encoding='utf-8')
    print(json.dumps(comparison,indent=2))

if __name__=='__main__':main()
