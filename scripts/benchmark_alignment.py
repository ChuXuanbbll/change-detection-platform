"""Same-machine before/after alignment benchmark with exact raster comparison."""
import ast
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT/'backend-py'))

def peak_memory():
    if os.name!='nt':
        import resource
        return resource.getrusage(resource.RUSAGE_SELF).ru_maxrss*1024
    import ctypes
    from ctypes import wintypes
    class Counters(ctypes.Structure):
        _fields_=[('cb',wintypes.DWORD),('faults',wintypes.DWORD)]+[(n,ctypes.c_size_t) for n in ('peak','working','pagedPeak','paged','nonpagedPeak','nonpaged','pagefile','pagefilePeak')]
    c=Counters();c.cb=ctypes.sizeof(c)
    kernel=ctypes.WinDLL('kernel32');kernel.GetCurrentProcess.restype=wintypes.HANDLE
    get_info=ctypes.WinDLL('psapi').GetProcessMemoryInfo
    get_info.argtypes=[wintypes.HANDLE,ctypes.POINTER(Counters),wintypes.DWORD]
    if not get_info(kernel.GetCurrentProcess(),ctypes.byref(c),c.cb):raise ctypes.WinError()
    return c.peak

def worker(mode,work):
    import rasterio
    import numpy as np
    from app.routers import detect
    align=detect._align_pair
    if mode=='before':
        source=(work/'baseline.py').read_text(encoding='utf-8')
        function=next(n for n in ast.parse(source).body if isinstance(n,ast.FunctionDef) and n.name=='_align_pair')
        namespace=dict(vars(detect))
        exec(compile(ast.Module(body=[function],type_ignores=[]),'baseline','exec'),namespace)
        align=namespace['_align_pair']
    output=work/mode;output.mkdir(exist_ok=True)
    started=time.perf_counter()
    files=align(str(work/'before.tif'),str(work/'after.tif'),output)
    elapsed=time.perf_counter()-started;peak=peak_memory()
    hashes=[]
    for file in files:
        with rasterio.open(file) as src:
            hashes.append(hashlib.sha256(src.read().tobytes()+src.dataset_mask().tobytes()).hexdigest())
    print(json.dumps({'seconds':elapsed,'peak_rss_bytes':peak,'disk_bytes':sum(Path(p).stat().st_size for p in files),'raster_hashes':hashes}))

def main():
    import numpy as np
    import rasterio
    from rasterio.transform import from_origin
    work=Path(tempfile.mkdtemp(prefix='alignment-',dir=ROOT/'data/validation'))
    baseline=subprocess.check_output(['git','show','17acd04:backend-py/app/routers/detect.py'],cwd=ROOT)
    (work/'baseline.py').write_bytes(baseline)
    rng=np.random.default_rng(81)
    for name,offset in [('before',0),('after',.3)]:
        with rasterio.open(work/f'{name}.tif','w',driver='GTiff',width=4096,height=4096,count=3,dtype='uint8',
            crs='EPSG:32650',transform=from_origin(350000+offset,4300000-offset,.8,.8)) as dst:
            dst.write(rng.integers(0,255,(3,4096,4096),dtype='uint8'))
            mask=np.full((4096,4096),255,dtype='uint8');mask[:40]=0;dst.write_mask(mask)
    results={}
    for mode in ('before','after'):
        results[mode]=json.loads(subprocess.check_output([sys.executable,'-X','utf8',__file__,mode,str(work)],cwd=ROOT,env=dict(os.environ,PYTHONWARNINGS='ignore::PendingDeprecationWarning')))
    assert results['before']['raster_hashes']==results['after']['raster_hashes'],results
    results['identical_pixels_and_masks']=True
    (work/'report.json').write_text(json.dumps(results,indent=2),encoding='utf-8')
    print(work/'report.json');print(json.dumps(results,indent=2))

if __name__=='__main__':
    if len(sys.argv)>1:worker(sys.argv[1],Path(sys.argv[2]))
    else:main()
