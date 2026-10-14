"""Isolated Go/Python acceptance using a real polygon crop and CUDA inference."""
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time

import numpy as np
import rasterio
import requests
import yaml
from rasterio.features import geometry_mask
from shapely.geometry import Polygon, box, mapping, shape

ROOT = Path(__file__).resolve().parents[1]


def port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def main():
    (ROOT / 'data/validation').mkdir(parents=True, exist_ok=True)
    work = Path(tempfile.mkdtemp(prefix='polygon-cuda-', dir=ROOT / 'data/validation'))
    pyport, goport = port(), port()
    env = dict(os.environ, DATA_DIR=str(work / 'data'), LOCAL_IMAGERY_DIR=str(ROOT / 'data/imagery'),
               GOCACHE=str(ROOT / '.cache/go-build'), GO_API_PORT=str(goport),
               PY_ALGO_URL=f'http://127.0.0.1:{pyport}', CONFIG_PATH=str(work / 'config.yaml'),
               PYTHONWARNINGS='ignore::PendingDeprecationWarning')
    config = yaml.safe_load((ROOT / 'backend-go/config.yaml').read_text(encoding='utf-8'))
    (work / 'config.yaml').write_text(yaml.safe_dump(config), encoding='utf-8')
    executable = work / 'server.exe'
    subprocess.run(['go', 'build', '-o', str(executable), './cmd/server'], cwd=ROOT / 'backend-go', env=env, check=True)
    processes, logs = [], []
    session = requests.Session()
    session.trust_env = False
    base = f'http://127.0.0.1:{goport}'

    def call(method, path, **kwargs):
        response = session.request(method, base + path, timeout=180, **kwargs)
        if response.status_code >= 400:
            raise RuntimeError(f'{response.status_code}: {response.text}')
        return response

    def launch(command, cwd, name):
        log = (work / f'{name}.log').open('w', encoding='utf-8')
        logs.append(log)
        process = subprocess.Popen(command, cwd=cwd, env=env, stdout=log, stderr=log,
                                   creationflags=subprocess.CREATE_NO_WINDOW if os.name == 'nt' else 0)
        processes.append(process)

    try:
        launch([str(ROOT / 'backend-py/.venv-local/Scripts/python.exe'), '-m', 'uvicorn', 'app.main:app',
                '--host', '127.0.0.1', '--port', str(pyport)], ROOT / 'backend-py', 'python')
        for _ in range(90):
            try:
                if session.get(env['PY_ALGO_URL'] + '/', timeout=2).ok:
                    break
            except requests.RequestException:
                pass
            time.sleep(.5)
        launch([str(executable)], ROOT / 'backend-go', 'go')
        state = None
        for _ in range(120):
            try:
                state = call('GET', '/api/settings').json()
                if state['effective']:
                    break
            except (RuntimeError, requests.RequestException):
                pass
            time.sleep(.5)
        assert state and state['effective'], state
        print('Isolated services ready; validating CUDA model', flush=True)
        operation = call('POST', '/api/models/builtin-hebei/validate', json={'mapping': {}}).json()
        while operation['status'] == 'running':
            time.sleep(.5)
            operation = call('GET', '/api/settings/operations/' + operation['id']).json()
        assert operation['status'] == 'succeeded', operation
        model = operation['result']
        assert model['status'] == 'ready', model
        catalog = session.get(env['PY_ALGO_URL'] + '/py/local/catalog', timeout=30).json()
        scenes = [row for row in catalog['images'] if row['id'].endswith('-guan')]
        assert len(scenes) >= 2, scenes
        administrative = session.get(env['PY_ALGO_URL'] + '/py/local/regions', timeout=30).json()
        region = shape(next(f['geometry'] for f in administrative['features'] if f['id'] == '131000'))
        overlap = region
        for scene in scenes:
            overlap = overlap.intersection(box(*scene['bounds']))
        center = overlap.representative_point()
        delta = .00025
        aoi_shape = Polygon([(center.x-delta, center.y-delta), (center.x+delta, center.y-delta),
                             (center.x-delta, center.y+delta)])
        assert region.covers(aoi_shape)
        aoi = json.loads(json.dumps(mapping(aoi_shape)))
        started = time.perf_counter()
        batch = call('POST', '/api/images/local', json={'bounds': list(aoi_shape.bounds), 'aoi': aoi,
            'region_id': '131000', 'scene_ids': [row['id'] for row in scenes], 'model_id': model['id'],
            'config_revision': state['revision'], 'params': {'resolution': .8, 'kernel_size': 1, 'min_area_pixels': 1}}).json()
        assert len(batch['items']) >= 2, batch
        crop_seconds = time.perf_counter() - started
        for image in batch['items']:
            meta = json.loads(image['meta_json'])
            assert meta['aoi'] == aoi and meta['region_id'] == '131000'
            with rasterio.open(image['file_path']) as src:
                inside = geometry_mask([aoi], src.shape, src.transform, invert=True)
                valid = src.dataset_mask() > 0
                assert valid.any() and (~valid).any()
                assert not valid[~inside].any()
                assert (src.read()[:, ~valid] == 0).all()
        print('Polygon crops and metadata verified; running real CUDA detection', flush=True)
        task = call('POST', '/api/tasks', json={'batch_id': batch['batch_id'], 'before_id': batch['items'][0]['id'],
                    'after_id': batch['items'][1]['id'], 'algorithm': 'hebei_onnx', 'params': {'provider': 'cuda'}}).json()
        started = time.perf_counter()
        while task['status'] in ('pending', 'running'):
            assert time.perf_counter() - started < 240, task
            time.sleep(.5)
            task = call('GET', '/api/tasks/' + task['id']).json()
        assert task['status'] == 'succeeded', task
        result = call('GET', '/api/tasks/' + task['id'] + '/result').json()
        stats = json.loads(result['stats_json'])
        with rasterio.open(result['mask_path']) as mask, rasterio.open(result['confidence_path']) as confidence:
            valid = mask.dataset_mask() > 0
            np.testing.assert_array_equal(valid, confidence.dataset_mask() > 0)
            inside = geometry_mask([aoi], mask.shape, mask.transform, invert=True)
            assert not valid[~inside].any()
            assert (mask.read(1)[~valid] == 0).all()
            assert (confidence.read(1)[~valid] == 0).all()
            assert stats['total_pixels'] == int(valid.sum())
            assert stats['changed_pixels'] == int(mask.read(1)[valid].sum())
            assert stats['confidence_histograms']['all_valid_sample_count'] == int(valid.sum())
        exports = {}
        for name in ('mask.tif', 'change.geojson', 'stats.csv', 'change.zip'):
            response = call('GET', f"/api/tasks/{task['id']}/result/{name}")
            assert response.content
            exports[name] = len(response.content)
        report = {'status': 'passed', 'provider': 'cuda', 'aoi': aoi, 'task_id': task['id'],
                  'crop_seconds': crop_seconds, 'detect_seconds': time.perf_counter()-started,
                  'stats': stats, 'exports': exports, 'result': result}
        (work / 'report.json').write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
        print(work / 'report.json', flush=True)
    finally:
        token = work / 'data/control.token'
        if token.exists():
            try:
                session.post(env['PY_ALGO_URL'] + '/py/admin/stop', headers={'X-CDP-Control': token.read_text()}, timeout=10)
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
