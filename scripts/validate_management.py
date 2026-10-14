"""Isolated real Windows GPU acceptance; never opens the development database."""
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import requests
import argparse
from shapely.geometry import box, shape

ROOT = Path(__file__).resolve().parents[1]

def port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]

def main():
    parser=argparse.ArgumentParser();parser.add_argument('--browser',action='store_true');args=parser.parse_args()
    work = Path(tempfile.mkdtemp(prefix='management-', dir=ROOT/'data/validation'))
    env = dict(os.environ, DATA_DIR=str(work/'data'), LOCAL_IMAGERY_DIR=str(ROOT/'data/imagery'),
               GOCACHE=str(ROOT/'backend-go/.cache/go-build'), PYTHONWARNINGS='ignore::PendingDeprecationWarning')
    pyport, goport, frontend_port = port(), port(), port()
    env.update(GO_API_PORT=str(goport), PY_ALGO_URL=f'http://127.0.0.1:{pyport}', CONFIG_PATH=str(ROOT/'backend-go/config.yaml'))
    import yaml
    config=yaml.safe_load((ROOT/'backend-go/config.yaml').read_text(encoding='utf-8'))
    config['cors_origins'].append(f'http://127.0.0.1:{frontend_port}')
    (work/'config.yaml').write_text(yaml.safe_dump(config),encoding='utf-8')
    env['CONFIG_PATH']=str(work/'config.yaml')
    executable = work/'server.exe'
    subprocess.run(['go','build','-o',str(executable),'./cmd/server'],cwd=ROOT/'backend-go',env=env,check=True)
    processes, logs = [], []
    session = requests.Session(); session.trust_env = False
    base = f'http://127.0.0.1:{goport}'
    def call(method,path,**kwargs):
        response=session.request(method,base+path,timeout=300,**kwargs)
        if response.status_code>=400: raise RuntimeError(f'{response.status_code} {response.text}')
        return response
    def operation(path,data):
        op=call('POST',path,json=data).json()
        while op['status']=='running':
            time.sleep(.5);op=call('GET','/api/settings/operations/'+op['id']).json()
        assert op['status']=='succeeded',op
        return op.get('result')
    def launch(command,cwd,name):
        log=(work/f'{name}.log').open('w',encoding='utf-8');logs.append(log)
        p=subprocess.Popen(command,cwd=cwd,env=env,stdout=log,stderr=log,creationflags=subprocess.CREATE_NO_WINDOW if os.name=='nt' else 0)
        processes.append(p);return p
    try:
        launch([str(ROOT/'backend-py/.venv-local/Scripts/python.exe'),'-m','uvicorn','app.main:app','--host','127.0.0.1','--port',str(pyport)],ROOT/'backend-py','python')
        for _ in range(90):
            try:
                if session.get(env['PY_ALGO_URL']+'/',timeout=2).ok: break
            except requests.RequestException: pass
            time.sleep(.5)
        launch([str(executable)],ROOT/'backend-go','go')
        for _ in range(120):
            try:
                state=call('GET','/api/settings').json()
                if state['effective']: break
            except (RuntimeError,requests.RequestException): pass
            time.sleep(.5)
        assert state['effective'],state
        model=operation('/api/models/builtin-hebei/validate',{'mapping':{}})
        assert model['status']=='ready',model
        # Stream the real 706 MB model through the public upload API; deduplicate by content.
        with (ROOT/'model-runtime/model/enforcement_2.onnx').open('rb') as stream:
            duplicate=call('POST','/api/models/import?name=duplicate',data=stream,headers={'Content-Type':'application/octet-stream'})
        assert duplicate.status_code==200 and duplicate.json()['id']==model['id']
        invalid=call('POST','/api/models/import?name=invalid',data=b'not an ONNX model',headers={'Content-Type':'application/octet-stream'}).json()
        while invalid['status']=='running':
            time.sleep(.2);invalid=call('GET','/api/settings/operations/'+invalid['id']).json()
        assert invalid['status']=='failed'
        registry=call('GET','/api/models').json()
        assert all(not m['enabled'] for m in registry if m['name']=='invalid')
        original=state['settings']
        revision=operation('/api/settings/apply',{'expected_revision':state['revision'],'settings':original})['revision']
        stale=session.post(base+'/api/settings/apply',json={'expected_revision':state['revision'],'settings':original})
        assert stale.status_code==409
        bad=json.loads(json.dumps(original));bad['sources'][0]['path']=str(work/'missing-drive')
        op=call('POST','/api/settings/apply',json={'expected_revision':revision,'settings':bad}).json()
        while op['status']=='running': time.sleep(.2);op=call('GET','/api/settings/operations/'+op['id']).json()
        assert op['status']=='failed'
        assert call('GET','/api/settings').json()['revision']==revision
        assert session.post(base+'/api/settings/sync',json={},headers={'Origin':'https://evil.example'}).status_code==403
        catalog=session.get(env['PY_ALGO_URL']+'/py/local/catalog',timeout=30).json()
        scenes=[r for r in catalog['images'] if r['id'].endswith('-guan')]
        overlap=shape(catalog['boundary'])
        for scene in scenes: overlap=overlap.intersection(box(*scene['bounds']))
        point=overlap.representative_point();delta=.00025
        bounds=[point.x-delta,point.y-delta,point.x+delta,point.y+delta]
        started=time.perf_counter()
        batch=call('POST','/api/images/local',json={'bounds':bounds,'region_id':'131000','scene_ids':[s['id'] for s in scenes], 'model_id':model['id'],'config_revision':revision,'params':{'resolution':.8}}).json()
        assert len(batch['items'])==2,batch
        crop_seconds=time.perf_counter()-started
        updated=json.loads(json.dumps(original));updated['detection']['threshold']=.9
        updated['tiles']['concurrency']=2;updated['tiles']['cache_mb']=32
        updated['runtime']['workers']=2;updated['runtime']['queue_capacity']=32
        operation('/api/settings/apply',{'expected_revision':revision,'settings':updated})
        actual=call('GET','/api/settings').json()['diagnostics']
        assert actual['queue']['workers']==2 and actual['queue']['capacity']==32
        assert actual['python']['tile_runtime']['concurrency']==2
        assert actual['python']['tile_runtime']['cache_bytes']==32*1024*1024
        task=call('POST','/api/tasks',json={'batch_id':batch['batch_id'],'before_id':batch['items'][0]['id'],'after_id':batch['items'][1]['id'],'params':{'threshold':.01}}).json()
        started=time.perf_counter()
        while task['status'] in ('pending','running'):
            time.sleep(.5);task=call('GET','/api/tasks/'+task['id']).json()
        detect_seconds=time.perf_counter()-started
        assert task['status']=='succeeded',task
        assert task['finished_at'],task
        result=call('GET',f"/api/tasks/{task['id']}/result").json()
        stats=json.loads(result['stats_json'])
        assert stats['execution']['sha256']==model['sha256']
        assert stats['execution']['config_revision']==revision
        assert stats['parameters']['threshold']==.5
        downloads={name:len(call('GET',f"/api/tasks/{task['id']}/result/{name}").content) for name in ('mask.tif','change.geojson','stats.csv','change.zip')}
        assert all(downloads.values())
        # Recover the persisted active revision in a fresh Go process.
        active=call('GET','/api/settings').json()
        server=processes[-1]
        server.terminate();server.wait(timeout=15)
        processes.remove(server)
        launch([str(executable)],ROOT/'backend-go','go-restarted')
        for _ in range(60):
            try:
                recovered=call('GET','/api/settings').json()
                if recovered['effective']:break
            except (RuntimeError,requests.RequestException):pass
            time.sleep(.5)
        assert recovered['revision']==active['revision'] and recovered['effective']
        reverted=operation('/api/settings/rollback',{'expected_revision':recovered['revision'],'rollback_revision':revision})
        assert call('GET','/api/settings').json()['settings']['detection']['threshold']==.5
        if args.browser:
            env.update(GO_PROXY_TARGET=base,PY_PROXY_TARGET=env['PY_ALGO_URL'],VITE_CACHE_DIR=str(work/'vite-cache'))
            launch(['node','node_modules/vite/bin/vite.js','--host','127.0.0.1','--port',str(frontend_port),'--strictPort'],ROOT/'frontend','vite')
            time.sleep(2)
            browser_env=dict(env,FRONTEND_URL=f'http://127.0.0.1:{frontend_port}',BROWSER_OUTPUT_DIR=str(work/'browser'))
            subprocess.run(['node','scripts/test_management_browser.mjs'],cwd=ROOT,env=browser_env,check=True,timeout=180)
        report={'status':'passed','provider':'cuda','model_hash':model['sha256'],'revision':revision,'crop_seconds':crop_seconds,'detect_seconds':detect_seconds,'result':result,'downloads':downloads,'tests':['version conflict','failed apply preserves active','origin rejected','large upload and hash dedup','invalid model disabled','batch freezes model and params','real GPU detection','exports','restart persistence','rollback','actual queue and tile settings']}
        (work/'report.json').write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding='utf-8')
        print(work/'report.json',flush=True)
    finally:
        if (work/'data/control.token').exists():
            try:session.post(env['PY_ALGO_URL']+'/py/admin/stop',headers={'X-CDP-Control':(work/'data/control.token').read_text()},timeout=10)
            except requests.RequestException:pass
        for p in reversed(processes):
            if os.name=='nt':subprocess.run(['taskkill','/PID',str(p.pid),'/T','/F'],capture_output=True)
            else:p.terminate()
            p.wait(timeout=15)
        for log in logs:log.close()

if __name__=='__main__':main()
