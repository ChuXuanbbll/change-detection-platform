"""Isolated real GPU progress acceptance; optionally run the workspace browser suite."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid

import requests
from shapely.geometry import box, shape
import yaml

from validate_management import ROOT, port


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--browser', action='store_true')
    args = parser.parse_args()
    work = Path(tempfile.mkdtemp(prefix='progress-', dir=ROOT/'data/validation'))
    pyport, goport, webport = port(), port(), port()
    env = dict(os.environ, DATA_DIR=str(work/'data'), LOCAL_IMAGERY_DIR=str(ROOT/'data/imagery'),
               GOCACHE=str(ROOT/'backend-go/.cache/go-build'), GO_API_PORT=str(goport),
               PY_ALGO_URL=f'http://127.0.0.1:{pyport}', PYTHONWARNINGS='ignore::PendingDeprecationWarning')
    config = yaml.safe_load((ROOT/'backend-go/config.yaml').read_text(encoding='utf-8'))
    config['cors_origins'].append(f'http://127.0.0.1:{webport}')
    (work/'config.yaml').write_text(yaml.safe_dump(config), encoding='utf-8')
    env['CONFIG_PATH'] = str(work/'config.yaml')
    executable = work/'server.exe'
    subprocess.run(['go', 'build', '-o', str(executable), './cmd/server'], cwd=ROOT/'backend-go', env=env, check=True)
    processes, logs = [], []
    session = requests.Session()
    session.trust_env = False
    base = f'http://127.0.0.1:{goport}'
    observations = {'preparation': [], 'task': []}

    def call(method, path, **kwargs):
        response = session.request(method, base+path, timeout=300, **kwargs)
        response.raise_for_status()
        return response.json()

    def launch(command, cwd, name):
        log = (work/f'{name}.log').open('w', encoding='utf-8')
        logs.append(log)
        process = subprocess.Popen(command, cwd=cwd, env=env, stdout=log, stderr=log,
                                   creationflags=subprocess.CREATE_NO_WINDOW if os.name == 'nt' else 0)
        processes.append(process)

    try:
        launch([str(ROOT/'backend-py/.venv-local/Scripts/python.exe'), '-m', 'uvicorn', 'app.main:app',
                '--host', '127.0.0.1', '--port', str(pyport)], ROOT/'backend-py', 'python')
        for _ in range(120):
            try:
                if session.get(env['PY_ALGO_URL']+'/', timeout=2).ok:
                    break
            except requests.RequestException:
                pass
            time.sleep(.5)
        launch([str(executable)], ROOT/'backend-go', 'go')
        for _ in range(120):
            try:
                state = call('GET', '/api/settings')
                if state['effective']:
                    break
            except requests.RequestException:
                pass
            time.sleep(.5)
        assert state['effective'], state
        operation = call('POST', '/api/models/builtin-hebei/validate', json={'mapping': {}})
        while operation['status'] == 'running':
            time.sleep(.5)
            operation = call('GET', '/api/settings/operations/'+operation['id'])
        assert operation['status'] == 'succeeded', operation
        model = operation['result']
        print('GPU model ready; preparing real multi-tile imagery', flush=True)
        catalog = session.get(env['PY_ALGO_URL']+'/py/local/catalog', timeout=60).json()
        scenes = [r for r in catalog['images'] if r['id'].endswith('-guan')]
        assert len(scenes) == 2, scenes
        overlap = shape(catalog['boundary'])
        for scene in scenes:
            overlap = overlap.intersection(box(*scene['bounds']))
        point, delta = overlap.representative_point(), .006
        progress_id = str(uuid.uuid4())
        payload = {'bounds': [point.x-delta, point.y-delta, point.x+delta, point.y+delta],
                   'region_id': '131000', 'scene_ids': [s['id'] for s in scenes],
                   'model_id': model['id'], 'config_revision': state['revision'],
                   'params': {'resolution': .8}, 'progress_id': progress_id}

        def prepare():
            with requests.Session() as preparation_session:
                preparation_session.trust_env = False
                response = preparation_session.post(base+'/api/images/local', json=payload, timeout=300)
                response.raise_for_status()
                return response.json()

        with ThreadPoolExecutor(max_workers=1) as pool:
            future = pool.submit(prepare)
            while not future.done():
                response = session.get(base+'/api/images/local/progress/'+progress_id, timeout=10)
                if response.ok:
                    observations['preparation'].append(response.json())
                else:
                    assert response.status_code == 404, response.text
                time.sleep(.2)
            batch = future.result()
        observations['preparation'].append(call('GET', '/api/images/local/progress/'+progress_id))
        assert observations['preparation'][-1]['status'] == 'succeeded'
        assert observations['preparation'][-1]['progress'] == 100
        # A crop can finish within one 500 ms sampling interval. The blocking
        # API fixture separately proves intermediate preparation visibility.
        assert any(p['status'] == 'running' for p in observations['preparation'])
        assert len(batch['items']) == 2
        task = call('POST', '/api/tasks', json={'batch_id': batch['batch_id'],
                    'before_id': batch['items'][0]['id'], 'after_id': batch['items'][1]['id'], 'params': {}})
        deadline = time.monotonic()+300
        while task['status'] in ('pending', 'running'):
            assert time.monotonic() < deadline, 'GPU detection timed out'
            observations['task'].append(task)
            time.sleep(.2)
            task = call('GET', '/api/tasks/'+task['id'])
        observations['task'].append(task)
        assert task['status'] == 'succeeded', task
        assert task['progress'] == 100 and task['stage'] == 'completed'
        percentages = [t['progress'] for t in observations['task']]
        assert percentages == sorted(percentages), percentages
        intermediate = sorted({t['progress'] for t in observations['task'] if t.get('stage') == 'inference' and 10 < t['progress'] < 85})
        assert len(intermediate) >= 2, intermediate
        result = call('GET', f"/api/tasks/{task['id']}/result")
        assert result['mask_path'] and result['geojson']
        print(f'GPU progress verified: {intermediate}', flush=True)
        if args.browser:
            env.update(GO_PROXY_TARGET=base, PY_PROXY_TARGET=env['PY_ALGO_URL'], VITE_CACHE_DIR=str(work/'vite-cache'))
            launch(['node', 'node_modules/vite/bin/vite.js', '--host', '127.0.0.1', '--port', str(webport), '--strictPort'], ROOT/'frontend', 'vite')
            for _ in range(40):
                try:
                    if session.get(f'http://127.0.0.1:{webport}', timeout=2).ok:
                        break
                except requests.RequestException:
                    pass
                time.sleep(.25)
            browser_env = dict(env, FRONTEND_URL=f'http://127.0.0.1:{webport}', BROWSER_OUTPUT_DIR=str(work/'browser'))
            subprocess.run(['node', 'scripts/test_workspace_browser.mjs'], cwd=ROOT, env=browser_env, check=True, timeout=180)
        report = {'status': 'passed', 'provider': 'cuda', 'intermediate_inference_percentages': intermediate,
                  'browser': args.browser, 'observations': observations}
        (work/'report.json').write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
        print(work/'report.json', flush=True)
    finally:
        (work/'observations.json').write_text(json.dumps(observations, ensure_ascii=False, indent=2), encoding='utf-8')
        if (work/'data/control.token').exists():
            try:
                session.post(env['PY_ALGO_URL']+'/py/admin/stop', headers={'X-CDP-Control': (work/'data/control.token').read_text()}, timeout=10)
            except requests.RequestException:
                pass
        for process in reversed(processes):
            if process.poll() is None:
                if os.name == 'nt':
                    subprocess.run(['taskkill', '/PID', str(process.pid), '/T', '/F'], capture_output=True)
                else:
                    process.terminate()
                process.wait(timeout=15)
        for log in logs:
            log.close()


if __name__ == '__main__':
    main()
