"""Bounded process rendering, versioned parent cache and cancellation-aware single flight."""
import asyncio
from collections import OrderedDict
from concurrent.futures import ProcessPoolExecutor
from concurrent.futures.process import BrokenProcessPool
from dataclasses import dataclass
from functools import partial
import multiprocessing
import os
from time import perf_counter

import anyio
from fastapi import HTTPException, Request, Response
from app.utils.tile_worker import initialize_worker, render_job, worker_ready


def busy(message='瓦片服务繁忙，请稍后重试'):
    return HTTPException(503, message, headers={'Retry-After': '1', 'Cache-Control': 'no-store'})


@dataclass
class Flight:
    job: object
    created: float
    waiters: int = 0
    started: bool = False
    task: asyncio.Task | None = None


class TileRenderer:
    def __init__(self, concurrency=4, max_pending=None, wait_seconds=1,
                 cache_items=512, cache_bytes=64 * 1024 * 1024, handles=8, gdal_cache_mb=128,
                 executor=None, worker=render_job):
        self.concurrency = concurrency
        self.max_pending = max_pending if max_pending is not None else concurrency * 2
        self.wait_seconds = wait_seconds
        self.slots = asyncio.Semaphore(concurrency)
        self.preparation = anyio.CapacityLimiter(1)
        self.requests = 0
        self.pool = executor
        self.worker = worker
        self.handles, self.gdal_cache_mb = handles, gdal_cache_mb
        self.cache_items, self.cache_bytes = cache_items, cache_bytes
        self.cache = OrderedDict()
        self.cached_bytes = 0
        self.flights = {}
        self.clients = 0
        self.closed = False
        self.pool_lock = asyncio.Lock()

    @property
    def pending(self):
        return len(self.flights)

    async def start(self):
        async with self.pool_lock:
            if self.closed:
                raise busy('瓦片服务正在关闭')
            if self.pool is not None:
                return
            ctx = multiprocessing.get_context('spawn')
            pool = ProcessPoolExecutor(max_workers=self.concurrency, mp_context=ctx,
                initializer=initialize_worker,
                initargs=(self.handles, self.gdal_cache_mb, ctx.Barrier(self.concurrency)))
            try:
                await asyncio.gather(*(asyncio.wrap_future(pool.submit(worker_ready))
                                       for _ in range(self.concurrency)))
            except BaseException:
                await anyio.to_thread.run_sync(partial(pool.shutdown, wait=True, cancel_futures=True))
                raise
            self.pool = pool

    def _store(self, key, data):
        if self.cache_items <= 0 or len(data) > self.cache_bytes:
            return
        old = self.cache.pop(key, None)
        if old is not None:
            self.cached_bytes -= len(old)
        self.cache[key] = data
        self.cached_bytes += len(data)
        while len(self.cache) > self.cache_items or self.cached_bytes > self.cache_bytes:
            self.cached_bytes -= len(self.cache.popitem(last=False)[1])

    def _finished(self, key, task):
        if self.flights.get(key) is not None and self.flights[key].task is task:
            self.flights.pop(key)
        # A disconnected final waiter must not leave an unobserved exception.
        if not task.cancelled():
            task.exception()

    async def _execute(self, flight):
        acquired = False
        try:
            remaining = self.wait_seconds - (perf_counter() - flight.created)
            if remaining <= 0:
                raise busy('瓦片等待超时，请稍后重试')
            try:
                await asyncio.wait_for(self.slots.acquire(), remaining)
            except TimeoutError:
                raise busy('瓦片等待超时，请稍后重试') from None
            acquired = True
            # Request tasks detach on disconnect/cancellation; native jobs never lose their slot.
            if flight.waiters == 0:
                raise asyncio.CancelledError()
            flight.started = True
            rendering = perf_counter()
            if self.pool is None:
                await self.start()
            pool = self.pool
            try:
                data, compute_ms = await asyncio.wrap_future(pool.submit(self.worker, flight.job))
            except BrokenProcessPool:
                async with self.pool_lock:
                    if self.pool is pool:
                        self.pool = None
                        await anyio.to_thread.run_sync(partial(pool.shutdown, wait=True, cancel_futures=True))
                raise busy('瓦片工作进程已重启，请重试') from None
            self._store(flight.job.key, data)
            return data, (rendering-flight.created)*1000, (perf_counter()-rendering)*1000, compute_ms
        finally:
            if acquired:
                self.slots.release()

    async def render(self, request, job, cache_control='no-cache', started=None):
        started = perf_counter() if started is None else started
        if self.closed:
            raise busy('瓦片服务正在关闭')
        if await request.is_disconnected():
            return Response(status_code=499, headers={'Cache-Control': 'no-store'})
        key = job.key
        data = self.cache.get(key)
        if data is not None:
            self.cache.move_to_end(key)
            return self._response(data, cache_control, 'hit', started, 0, 0, 0)
        # Bound duplicate waiters too, while counting distinct work against the render queue.
        if self.clients >= self.max_pending * 8:
            raise busy()
        await self.start()
        self.clients += 1
        flight = None
        waiting = perf_counter()
        try:
            while True:
                if self.closed:
                    raise busy('瓦片服务正在关闭')
                if await request.is_disconnected():
                    return Response(status_code=499, headers={'Cache-Control': 'no-store'})
                data = self.cache.get(key)
                if data is not None:
                    self.cache.move_to_end(key)
                    return self._response(data, cache_control, 'hit', started, 0, 0, 0)
                flight = self.flights.get(key)
                status = 'shared' if flight else 'miss'
                if flight is not None or self.pending < self.max_pending:
                    if flight is None:
                        flight = Flight(job, waiting)
                        self.flights[key] = flight
                        flight.task = asyncio.create_task(self._execute(flight))
                        flight.task.add_done_callback(partial(self._finished, key))
                    flight.waiters += 1
                    break
                # Aborted requests and native work release capacity asynchronously.
                # Backpressure within the same queue deadline instead of turning
                # a brief pan overlap into a Retry-After round trip.
                remaining = self.wait_seconds - (perf_counter() - waiting)
                if remaining <= 0:
                    raise busy()
                await asyncio.sleep(min(.01, remaining))
            while not flight.task.done():
                if await request.is_disconnected():
                    return Response(status_code=499, headers={'Cache-Control': 'no-store'})
                await asyncio.wait({flight.task}, timeout=.01)
            data, queue_ms, render_ms, compute_ms = flight.task.result()
            return self._response(data, cache_control, status, started, queue_ms, render_ms, compute_ms)
        finally:
            self.clients -= 1
            if flight is not None:
                flight.waiters -= 1
                if flight.waiters == 0 and not flight.started and not flight.task.done():
                    flight.task.cancel()
                    if self.flights.get(key) is flight:
                        self.flights.pop(key)

    @staticmethod
    def _response(data, cache_control, status, started, queue_ms, render_ms, compute_ms):
        return Response(data, media_type='image/png', headers={
            'Cache-Control': cache_control, 'X-Tile-Cache': status,
            'Server-Timing': f'tile_queue;dur={queue_ms:.2f}, tile_render;dur={render_ms:.2f}, '
                             f'tile_compute;dur={compute_ms:.2f}, tile_total;dur={(perf_counter()-started)*1000:.2f}'})

    async def close(self):
        self.closed = True
        tasks = [flight.task for flight in self.flights.values()]
        for flight in self.flights.values():
            if not flight.started:
                flight.task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        async with self.pool_lock:
            if self.pool is not None:
                await anyio.to_thread.run_sync(partial(self.pool.shutdown, wait=True, cancel_futures=True))
                self.pool = None
        self.cache.clear()
        self.cached_bytes = 0


def configured_renderer():
    def number(name, default, lower, upper):
        return max(lower, min(upper, int(os.environ.get(name, default))))
    concurrency = number('TILE_RENDER_CONCURRENCY', 4, 1, 8)
    return TileRenderer(concurrency=concurrency,
        max_pending=number('TILE_MAX_PENDING', concurrency * 2, concurrency, 64),
        wait_seconds=number('TILE_QUEUE_WAIT_MS', 1000, 5, 1000)/1000,
        handles=number('TILE_DATASET_HANDLES', 8, 1, 32),
        gdal_cache_mb=number('TILE_GDAL_CACHE_MB', 128, 16, 512))


async def tile_response(request: Request, prepare, cache_control='no-cache'):
    state = request.app.state
    if not hasattr(state, 'tile_renderer'):
        state.tile_renderer = configured_renderer()
    renderer = state.tile_renderer
    # Catalogue discovery can access SMB. Keep it outside the event loop and validate
    # versions before every cache hit. Only rendering crosses the process boundary.
    if renderer.requests >= renderer.max_pending * 2:
        raise busy()
    renderer.requests += 1
    started = perf_counter()
    def checked_prepare():
        if perf_counter() - started > renderer.wait_seconds:
            raise busy('瓦片校验等待超时，请稍后重试')
        return prepare()
    try:
        if await request.is_disconnected():
            return Response(status_code=499, headers={'Cache-Control': 'no-store'})
        job = await anyio.to_thread.run_sync(checked_prepare, limiter=renderer.preparation)
        prepared_ms = (perf_counter() - started) * 1000
        response = await renderer.render(request, job, cache_control, started=started)
        if 'Server-Timing' in response.headers:
            response.headers['Server-Timing'] += f', tile_prepare;dur={prepared_ms:.2f}'
        return response
    finally:
        renderer.requests -= 1
