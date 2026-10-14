"""Compare a Git baseline and working tree against the same read-only imagery snapshot.

Starts isolated Python services, aborts obsolete view requests, then loads a final
viewport with the browser's four-request limit and Retry-After behavior.
"""
import argparse
import asyncio
from collections import Counter
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
from urllib.parse import urlencode

import requests

ROOT = Path(__file__).resolve().parents[1]


async def measure(port, phase, region):
    query = urlencode({'scene_ids': ','.join(row['id'] for row in phase['scenes']),
                       'v': phase['version']})
    statuses = Counter()
    aborted = 0
    timings = []

    async def request(x, y, abort_after=None):
        nonlocal aborted
        reader, writer = await asyncio.open_connection('127.0.0.1', port)
        path = f'/py/local/regions/{region}/mosaic/16/{x}/{y}.png?{query}'
        writer.write(f'GET {path} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n'.encode())
        await writer.drain()
        try:
            async with asyncio.timeout(abort_after or 20):
                data = await reader.read()
            header = data.split(b'\r\n\r\n', 1)[0].decode()
            status = int(header.split()[1])
            statuses[status] += 1
            for line in header.splitlines():
                if line.lower().startswith('server-timing:'):
                    timings.append(line)
            return status
        except TimeoutError:
            if abort_after is None:
                raise
            aborted += 1
        finally:
            writer.close()
            await writer.wait_closed()

    # Four successive views abandoned while native GDAL jobs are still running.
    for view in range(4):
        await asyncio.gather(*(request(53864 + view, 25066 + i, .04) for i in range(4)))
    stopped = time.perf_counter()
    limit = asyncio.Semaphore(4)

    async def tile(i):
        for attempt in range(5):
            async with limit:
                status = await request(53869 + i % 6, 25065 + i // 6)
            if status not in (429, 503) or attempt == 4:
                return status
            await asyncio.sleep(max(1, .25 * 2**attempt))

    final = await asyncio.gather(*(tile(i) for i in range(24)))
    return {'http_statuses': dict(statuses), 'aborted': aborted,
            'final_tiles': dict(Counter(final)),
            'final_view_ms': round((time.perf_counter() - stopped) * 1000),
            'server_timings': timings}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--baseline', default='76b3d8c')
    parser.add_argument('--catalog-url', default='http://127.0.0.1:8090')
    args = parser.parse_args()
    session = requests.Session()
    session.trust_env = False
    response = session.get(args.catalog_url + '/py/local/regions/xiongan/scenes', timeout=30)
    response.raise_for_status()
    snapshot = response.json()
    phase = next(p for p in snapshot['phases'] if p['available'])
    output = Path(tempfile.mkdtemp(prefix='tile-pan-', dir=ROOT / 'data/validation'))
    report = {'baseline_ref': args.baseline, 'region': 'xiongan', 'phase': phase['id']}
    for name in ('baseline', 'fixed'):
        folder = output / name
        shutil.copytree(ROOT / 'backend-py/app', folder / 'app',
                        ignore=shutil.ignore_patterns('__pycache__'))
        if name == 'baseline':
            for relative in ('main.py', 'settings.py', 'fetchers/local.py',
                             'utils/tile_queue.py', 'utils/tile_worker.py'):
                data = subprocess.check_output(['git', 'show', f'{args.baseline}:backend-py/app/{relative}'], cwd=ROOT)
                (folder / 'app' / relative).write_bytes(data)
        (folder / 'rows.json').write_text(json.dumps(snapshot['scenes']), encoding='utf-8')
        (folder / 'benchmark_app.py').write_text(
            "import json\nfrom pathlib import Path\nfrom app.fetchers import local\n"
            "rows = json.loads(Path(__file__).with_name('rows.json').read_text(encoding='utf-8'))\n"
            "local._configured_catalog = lambda: rows\nfrom app.main import app\n", encoding='utf-8')
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        env = dict(os.environ, ASSET_DIR=str(ROOT / 'data'), DATA_DIR=str(folder / 'data'),
                   PYTHONPATH=str(folder), PYTHONWARNINGS='ignore::PendingDeprecationWarning')
        for key in ('TILE_QUEUE_WAIT_MS', 'TILE_RENDER_CONCURRENCY', 'TILE_MAX_PENDING'):
            env.pop(key, None)
        with (folder / 'server.log').open('w', encoding='utf-8') as log:
            process = subprocess.Popen([sys.executable, '-m', 'uvicorn', 'benchmark_app:app',
                                        '--host', '127.0.0.1', '--port', str(port)],
                                       cwd=folder, env=env, stdout=log, stderr=log,
                                       start_new_session=os.name != 'nt',
                                       creationflags=subprocess.CREATE_NO_WINDOW if os.name == 'nt' else 0)
            try:
                for _ in range(120):
                    if process.poll() is not None:
                        raise RuntimeError((folder / 'server.log').read_text(encoding='utf-8'))
                    try:
                        if session.get(f'http://127.0.0.1:{port}/', timeout=1).ok:
                            break
                    except requests.RequestException:
                        pass
                    time.sleep(.1)
                report[name] = asyncio.run(measure(port, phase, 'xiongan'))
                print(name, json.dumps({k: v for k, v in report[name].items() if k != 'server_timings'}), flush=True)
            finally:
                # Stop only the isolated service and its owned process pool.
                if process.poll() is None:
                    if os.name == 'nt':
                        subprocess.run(['taskkill', '/PID', str(process.pid), '/T', '/F'],
                                       check=True, stdout=subprocess.DEVNULL,
                                       creationflags=subprocess.CREATE_NO_WINDOW)
                    else:
                        os.killpg(process.pid, signal.SIGTERM)
                    process.wait(timeout=10)
    (output / 'report.json').write_text(json.dumps(report, indent=2), encoding='utf-8')
    print(output / 'report.json')
    assert report['fixed']['final_tiles'] == {200: 24}, report['fixed']
    assert report['fixed']['http_statuses'].get(503, 0) == 0, report['fixed']


if __name__ == '__main__':
    main()
