import asyncio
from concurrent.futures import ThreadPoolExecutor
import threading
import unittest
from fastapi import HTTPException
from app.utils.tile_queue import TileRenderer
from app.utils.tile_worker import TileJob


class Connection:
    disconnected = False
    async def is_disconnected(self):
        return self.disconnected


def job(x=0, version='v1', generation=0):
    return TileJob(({'id':'scene', 'version':version},), None, 2, x, 0, (generation,))


class TileQueueTests(unittest.IsolatedAsyncioTestCase):
    async def test_full_queue_waits_for_aborted_view_without_exceeding_capacity(self):
        started, release = threading.Event(), threading.Event()
        self.addCleanup(release.set)
        calls = []
        def worker(task):
            calls.append(task.x)
            if task.x == 0:
                started.set()
                if not release.wait(3):
                    raise TimeoutError()
            return b'png', 1
        r = self.renderer(worker, concurrency=1, max_pending=2)
        first = asyncio.create_task(r.render(Connection(), job()))
        await self.wait_for(started.is_set)
        old = Connection()
        queued = asyncio.create_task(r.render(old, job(1)))
        await self.wait_for(lambda: r.pending == 2)
        fresh = asyncio.create_task(r.render(Connection(), job(2)))
        await self.wait_for(lambda: r.clients == 3)
        self.assertEqual(r.pending, 2)
        old.disconnected = True
        self.assertEqual((await queued).status_code, 499)
        await self.wait_for(lambda: job(2).key in r.flights)
        self.assertLessEqual(r.pending, 2)
        release.set()
        self.assertEqual((await fresh).status_code, 200)
        await first
        self.assertEqual(calls, [0, 2])

    async def test_default_wait_absorbs_running_work_after_pan(self):
        started, release = threading.Event(), threading.Event()
        self.addCleanup(release.set)
        def worker(task):
            if task.x == 0:
                started.set()
                if not release.wait(3):
                    raise TimeoutError()
            return b'png', 1
        r = self.renderer(worker, concurrency=1)
        old = Connection()
        first = asyncio.create_task(r.render(old, job()))
        await self.wait_for(started.is_set)
        old.disconnected = True
        self.assertEqual((await first).status_code, 499)
        fresh = asyncio.create_task(r.render(Connection(), job(1)))
        await asyncio.sleep(.1)  # Typical raster IO exceeds the former 25 ms budget.
        release.set()
        self.assertEqual((await fresh).status_code, 200)

    async def wait_for(self, predicate):
        async def poll():
            while not predicate(): await asyncio.sleep(.002)
        await asyncio.wait_for(poll(), 3)

    def renderer(self, worker, **kwargs):
        renderer = TileRenderer(executor=ThreadPoolExecutor(max_workers=2), worker=worker, **kwargs)
        self.addAsyncCleanup(renderer.close)
        return renderer

    async def test_disconnected_queue_never_submits(self):
        started, release = threading.Event(), threading.Event()
        self.addCleanup(release.set)
        calls=[]
        def worker(task):
            calls.append(task.x)
            if task.x==0:
                started.set()
                if not release.wait(3): raise TimeoutError()
            return b'png', 1
        r=self.renderer(worker,concurrency=1,wait_seconds=2)
        first=asyncio.create_task(r.render(Connection(),job()))
        await self.wait_for(started.is_set)
        old=Connection();queued=asyncio.create_task(r.render(old,job(1)))
        await self.wait_for(lambda:r.pending==2)
        old.disconnected=True
        self.assertEqual((await queued).status_code,499)
        release.set();await first
        await self.wait_for(lambda:r.pending==0)
        self.assertEqual(calls,[0])
        self.assertEqual((await r.render(Connection(),job(1))).body,b'png')

    async def test_running_cancellation_keeps_native_slot(self):
        started,release=threading.Event(),threading.Event();calls=[]
        self.addCleanup(release.set)
        def worker(task):
            calls.append(task.x)
            if task.x==0:
                started.set()
                if not release.wait(3):raise TimeoutError()
            return b'png',1
        r=self.renderer(worker,concurrency=1,wait_seconds=2)
        first=asyncio.create_task(r.render(Connection(),job()))
        await self.wait_for(started.is_set)
        first.cancel()
        with self.assertRaises(asyncio.CancelledError):await first
        fresh=asyncio.create_task(r.render(Connection(),job(1)))
        await asyncio.sleep(.03)
        self.assertEqual(calls,[0]);self.assertEqual(r.pending,2)
        release.set();await fresh
        self.assertEqual(calls,[0,1])

    async def test_single_flight_survives_one_waiter_disconnect_and_caches(self):
        started,release=threading.Event(),threading.Event();calls=[]
        self.addCleanup(release.set)
        def worker(task):
            calls.append(task.key);started.set()
            if not release.wait(3):raise TimeoutError()
            return b'png',1
        r=self.renderer(worker,concurrency=1,wait_seconds=2)
        old=Connection();first=asyncio.create_task(r.render(old,job()))
        await self.wait_for(started.is_set)
        second=asyncio.create_task(r.render(Connection(),job()))
        await self.wait_for(lambda:r.clients==2)
        self.assertEqual(r.pending,1)
        old.disconnected=True;self.assertEqual((await first).status_code,499)
        release.set()
        response=await second
        self.assertEqual(response.headers['x-tile-cache'],'shared')
        self.assertIn('tile_compute;',response.headers['server-timing'])
        self.assertEqual((await r.render(Connection(),job())).headers['x-tile-cache'],'hit')
        self.assertEqual(len(calls),1)

    async def test_short_deadline_and_bounded_work(self):
        started,release=threading.Event(),threading.Event()
        self.addCleanup(release.set)
        def worker(task):
            started.set()
            if not release.wait(3):raise TimeoutError()
            return b'png',1
        r=self.renderer(worker,concurrency=1,max_pending=2,wait_seconds=.03)
        first=asyncio.create_task(r.render(Connection(),job()))
        await self.wait_for(started.is_set)
        second=asyncio.create_task(r.render(Connection(),job(1)))
        await self.wait_for(lambda:r.pending==2)
        with self.assertRaises(HTTPException) as error:await r.render(Connection(),job(2))
        self.assertEqual(error.exception.status_code,503)
        with self.assertRaises(HTTPException) as error:await second
        self.assertEqual(error.exception.headers['Retry-After'],'1')
        release.set();await first
        self.assertEqual((await r.render(Connection(),job(2))).body,b'png')

    async def test_failures_not_cached_and_byte_budget_is_bounded(self):
        calls=[]
        def worker(task):
            calls.append(task.key)
            if task.x==3:raise ValueError('bad tile')
            return bytes([task.x])*3,1
        r=self.renderer(worker,cache_bytes=6,cache_items=10,wait_seconds=2)
        for x in [0,1,2]:await r.render(Connection(),job(x))
        self.assertEqual(r.cached_bytes,6);self.assertEqual(len(r.cache),2)
        await r.render(Connection(),job(0))
        self.assertEqual(len(calls),4)
        for _ in range(2):
            with self.assertRaises(ValueError):await r.render(Connection(),job(3))
        self.assertEqual(len(calls),6)

    async def test_version_and_refresh_generation_miss_parent_cache(self):
        calls=[]
        def worker(task):calls.append(task.key);return b'png',1
        r=self.renderer(worker,wait_seconds=2)
        for task in [job(),job(),job(version='v2'),job(version='v2',generation=1)]:
            await r.render(Connection(),task)
        self.assertEqual(len(calls),3)

    async def test_concurrency_cap(self):
        release=threading.Event();lock=threading.Lock();active=maximum=started=0
        self.addCleanup(release.set)
        def worker(task):
            nonlocal active,maximum,started
            with lock:active+=1;started+=1;maximum=max(maximum,active)
            try:
                if not release.wait(3):raise TimeoutError()
                return b'png',1
            finally:
                with lock:active-=1
        r=self.renderer(worker,concurrency=2,wait_seconds=2)
        tasks=[asyncio.create_task(r.render(Connection(),job(x))) for x in range(4)]
        await self.wait_for(lambda:started==2 and r.pending==4)
        release.set();await asyncio.gather(*tasks)
        self.assertEqual(maximum,2)
