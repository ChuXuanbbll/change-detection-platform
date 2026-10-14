"""Best-effort telemetry; output files and HTTP responses remain authoritative."""
import json
import os
import time
from pathlib import Path


class Progress:
    def __init__(self, output_dir):
        self.path = Path(output_dir) / 'progress' / 'stage.json'
        self.last_stage = None
        self.last_write = 0.0

    def __call__(self, stage, progress, *, force=False):
        now = time.monotonic()
        if not force and stage == self.last_stage and now - self.last_write < .25:
            return
        try:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            temporary = self.path.with_suffix('.tmp')
            temporary.write_text(json.dumps({'stage': stage, 'progress': progress}), encoding='utf-8')
            os.replace(temporary, self.path)
            self.last_stage, self.last_write = stage, now
        except OSError:
            # Telemetry must never fail an otherwise valid computation.
            pass
