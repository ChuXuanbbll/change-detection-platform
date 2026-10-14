"""Owned, bounded model processes; no shell commands or global process-name kills."""

from functools import lru_cache
from pathlib import Path
import os
import shutil
import subprocess
import sys
from threading import Lock

_running = set()
_accepting = True
_lock = Lock()


@lru_cache(maxsize=1)
def interpreter():
    from app.settings import PROJECT

    configured = os.getenv('CHANGE_ONNX_PYTHON')
    if configured:
        return str(Path(configured).resolve())
    candidate = (
        PROJECT
        / 'model-runtime/.venv-gpu'
        / ('Scripts/python.exe' if os.name == 'nt' else 'bin/python')
    )
    return str(candidate if candidate.exists() else Path(sys.executable))


@lru_cache(maxsize=1)
def cuda_directory():
    configured = os.getenv('CHANGE_CUDA_DLL_DIR')
    if configured or os.name != 'nt':
        return configured or ''
    roots = [Path(interpreter()).parent.parent]
    conda = shutil.which('conda')
    if conda:
        try:
            base = Path(
                subprocess.run(
                    [conda, 'info', '--base'],
                    capture_output=True,
                    text=True,
                    timeout=10,
                    check=True,
                ).stdout.strip()
            )
            roots += [base, *list((base / 'envs').glob('*'))]
        except (OSError, subprocess.SubprocessError):
            pass
    required = [
        'cublasLt64_12.dll',
        'cublas64_12.dll',
        'cufft64_11.dll',
        'cudart64_12.dll',
        'cudnn64_9.dll',
    ]
    for root in roots:
        for directory in (root / 'Lib/site-packages/torch/lib', root / 'Library/bin'):
            if all((directory / name).exists() for name in required):
                return str(directory)
    return ''


def terminate(process):
    if process.poll() is not None:
        return
    if os.name == 'nt':
        subprocess.run(
            ['taskkill', '/PID', str(process.pid), '/T', '/F'], capture_output=True, timeout=15
        )
    else:
        import signal

        os.killpg(process.pid, signal.SIGKILL)


def stop_all():
    global _accepting
    with _lock:
        _accepting = False
        processes = list(_running)
    for process in processes:
        terminate(process)


def run(command, *, timeout=3600, env=None, **unused):
    environment = dict(os.environ, **(env or {}))
    runtime = cuda_directory()
    if runtime:
        environment['CHANGE_CUDA_DLL_DIR'] = runtime
        environment['PATH'] = runtime + os.pathsep + environment.get('PATH', '')
    options = (
        {'creationflags': subprocess.CREATE_NO_WINDOW | subprocess.CREATE_NEW_PROCESS_GROUP}
        if os.name == 'nt'
        else {'start_new_session': True}
    )
    with _lock:
        if not _accepting:
            raise RuntimeError('模型服务正在关闭')
        process = subprocess.Popen(
            command,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            encoding='utf-8',
            errors='replace',
            env=environment,
            **options,
        )
        _running.add(process)
    try:
        try:
            stdout, stderr = process.communicate(timeout=timeout)
        except subprocess.TimeoutExpired:
            terminate(process)
            process.communicate()
            raise TimeoutError(f'模型执行超过 {timeout} 秒，进程已终止') from None
        return subprocess.CompletedProcess(command, process.returncode, stdout, stderr)
    finally:
        with _lock:
            _running.discard(process)


@lru_cache(maxsize=32)
def _hash_file(path, size, modified):
    import hashlib

    with Path(path).open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def verify_hash(path, expected):
    stat = path.stat()
    if _hash_file(str(path), stat.st_size, stat.st_mtime_ns) != expected:
        raise ValueError('模型哈希与批次快照不符，禁止执行')
