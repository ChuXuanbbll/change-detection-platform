import type L from 'leaflet';

export const TILE_LOAD_DEFAULTS = { concurrency: 4 };
type Job = { priority: () => number; run: () => Promise<void>; background?: boolean };

/** One queue across phases. Movement coalesces by frame, never waits for settling. */
export class TileScheduler {
  ready = false;
  moving = false;
  private disposed = false;
  private running = 0;
  private backgrounds = 0;
  private jobs = new Map<object, Job>();
  private frame?: number;
  private scheduled = false;

  constructor(private map: L.Map, private refresh: () => void,
    private limits = TILE_LOAD_DEFAULTS) {
    map.on('movestart zoomstart', this.begin, this);
    map.on('move zoom resize', this.changed, this);
    map.on('moveend', this.end, this);
  }

  private begin() { this.moving = true; this.changed(); }
  private end() { this.moving = false; this.changed(); }
  private changed() {
    if (!this.ready || this.disposed || this.frame !== undefined) return;
    this.frame = requestAnimationFrame(() => {
      this.frame = undefined;
      if (!this.ready || this.disposed) return;
      this.refresh();
      this.kick();
    });
  }

  hold() {
    this.ready = false;
    if (this.frame !== undefined) cancelAnimationFrame(this.frame);
    this.frame = undefined;
  }

  defer() {
    if (this.disposed) return;
    this.ready = true;
    this.refresh();
    this.kick();
  }

  enqueue(key: object, job: Job) {
    if (this.disposed) return;
    this.jobs.set(key, job);
    this.kick();
  }
  cancel(key: object) { this.jobs.delete(key); }
  kick() {
    if (this.scheduled || this.disposed) return;
    this.scheduled = true;
    queueMicrotask(() => { this.scheduled = false; this.drain(); });
  }
  private drain() {
    while (!this.disposed && this.ready && this.running < this.limits.concurrency) {
      let next: object | undefined, priority = Infinity;
      for (const [key, job] of this.jobs) {
        if (job.background && (this.moving || this.backgrounds >= 1)) continue;
        const value = job.priority();
        if (value < priority) { next = key; priority = value; }
      }
      if (!next) return;
      const job = this.jobs.get(next)!;
      this.jobs.delete(next);
      this.running++;
      if (job.background) this.backgrounds++;
      void job.run().catch(() => { /* The owner reports errors. */ }).finally(() => {
        this.running--;
        if (job.background) this.backgrounds--;
        this.kick();
      });
    }
  }
  dispose() {
    this.disposed = true;
    this.hold();
    this.jobs.clear();
    this.map.off('movestart zoomstart', this.begin, this);
    this.map.off('move zoom resize', this.changed, this);
    this.map.off('moveend', this.end, this);
  }
}
