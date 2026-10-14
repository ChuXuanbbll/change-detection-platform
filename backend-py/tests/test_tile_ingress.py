"""Exercise tile disconnects through the production middleware stack."""
import asyncio
from concurrent.futures import ThreadPoolExecutor
import threading
import tempfile
from pathlib import Path
import unittest
from unittest.mock import patch

from fastapi import Request
from fastapi.responses import JSONResponse
from app.main import app, PrivateComputeMiddleware, private_compute_routes
from app.utils.tile_queue import TileRenderer
from test_tile_queue import Connection, job


class TileIngressTests(unittest.IsolatedAsyncioTestCase):
    async def test_private_routes_still_validate_tokens_paths_and_replay_body(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            (root / 'control.token').write_text('test-token')
            for path, payload, token, expected in (
                ('/py/admin/apply', {}, '', 403),
                ('/py/detect', {}, '', 403),
                ('/py/local/fetch', {}, '', 403),
                ('/py/wayback/fetch', {}, '', 403),
                ('/py/thumbnail', {}, '', 403),
                ('/py/overlay/image', {'path': str(root.parent / 'outside')}, '', 400),
                ('/py/local/fetch', {'output_dir': str(root / 'batch')}, 'test-token', 200),
            ):
                with self.subTest(path=path, token=token):
                    import json
                    sent, messages, received = False, [], []
                    async def receive():
                        nonlocal sent
                        if not sent:
                            sent = True
                            return {'type': 'http.request', 'body': json.dumps(payload).encode()}
                        await asyncio.Event().wait()
                    async def send(message):
                        messages.append(message)
                    async def inner(scope, receive, send):
                        received.append(await Request(scope, receive).json())
                        await JSONResponse({'ok': True})(scope, receive, send)
                    middleware = PrivateComputeMiddleware(inner, dispatch=private_compute_routes)
                    scope = {'type': 'http', 'asgi': {'version': '3.0'}, 'http_version': '1.1',
                             'method': 'POST', 'scheme': 'http', 'path': path, 'query_string': b'',
                             'headers': [(b'x-cdp-control', token.encode())]}
                    with patch('app.settings.DATA_ROOT', root):
                        await middleware(scope, receive, send)
                    self.assertEqual(messages[0]['status'], expected)
                    self.assertEqual(received, [payload] if expected == 200 else [])

    async def test_disconnected_http_waiter_releases_queue_before_render_deadline(self):
        started, release = threading.Event(), threading.Event()
        calls = []

        def worker(task):
            calls.append(task.x)
            started.set()
            if not release.wait(3):
                raise TimeoutError()
            return b'png', 1

        renderer = TileRenderer(concurrency=1, max_pending=2, wait_seconds=2,
                                executor=ThreadPoolExecutor(1), worker=worker)
        disconnected = asyncio.Event()
        sent = False

        async def receive():
            nonlocal sent
            if not sent:
                sent = True
                return {'type': 'http.request', 'body': b'', 'more_body': False}
            await disconnected.wait()
            return {'type': 'http.disconnect'}

        async def send(message):
            pass

        scope = {'type': 'http', 'asgi': {'version': '3.0'}, 'http_version': '1.1',
                 'method': 'GET', 'scheme': 'http', 'path': '/py/local/tiles/latest/2/1/0.png',
                 'raw_path': b'/py/local/tiles/latest/2/1/0.png', 'query_string': b'',
                 'headers': [], 'server': ('127.0.0.1', 8090), 'client': ('127.0.0.1', 1234)}
        first = asyncio.create_task(renderer.render(Connection(), job()))
        request = None
        try:
            async with asyncio.timeout(2):
                while not started.is_set():
                    await asyncio.sleep(.002)
            with patch.object(app.state, 'tile_renderer', renderer, create=True), \
                    patch('app.routers.wayback.prepare_job', return_value=job(1)):
                request = asyncio.create_task(app(scope, receive, send))
                async with asyncio.timeout(2):
                    while renderer.clients != 2:
                        await asyncio.sleep(.002)
                disconnected.set()
                await asyncio.sleep(.15)
                self.assertEqual(renderer.clients, 1, 'aborted HTTP tile still occupies admission')
                self.assertEqual(renderer.pending, 1, 'aborted tile still occupies render queue')
                release.set()
                await request
                await first
                self.assertEqual(calls, [0], 'disconnected queued tile must never render')
        finally:
            release.set()
            if request is not None:
                await request
            await first
            await renderer.close()
