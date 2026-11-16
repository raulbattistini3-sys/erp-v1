// polyvisor/runtime: the service-side half of polyvisor. A service started by polyvisor (POLYVISOR=1)
// learns its limits from the environment, reads its real resource use from the OS (native.js) and decides
// how much work to take on (admission.js). It works the same outside polyvisor: limits then come from the
// job object / cgroup the process happens to be in, or there are none and only latency and the system's
// memory count.
//
//   const adaptive = createAdaptive();            // calibrated from data/measurements.pdf by default
//   adaptive.start();
//   app.use(adaptive.middleware());               // Express/connect: 503 + Retry-After when shedding
//   await adaptive.run("background", () => job()); // queue consumers, imports
//   adaptive.observe("db select produtos", ms);   // latency of things with a baseline
//   hostTelemetry({ extraSamples: adaptive.samples }) // adaptive.pressure samples in each envelope
import { createAdmission, Shed, CLASSES } from "./admission.js";
import { measured, thresholds as defaultThresholds } from "./baseline.js";
import * as native from "./native.js";

export { Shed, CLASSES, createAdmission, measured, native };
export { thresholds } from "./baseline.js";

/** @typedef {import("./admission.js").WorkClass} WorkClass */
/** @typedef {import("./admission.js").Level} Level */
/** @typedef {import("./baseline.js").Thresholds} Thresholds */

const LEVELS = /** @type {const} */ (["normal", "elevated", "critical"]);
const rank = (/** @type {Level} */ l) => LEVELS.indexOf(l);

/**
 * @typedef {object} Readings
 * @property {number} memoryBytes what counts against the limit (the job's/cgroup's total, else private bytes)
 * @property {number} memoryLimitBytes the hard limit (0: none known)
 * @property {number} memorySoftBytes
 * @property {number} systemLoadPercent
 * @property {number} systemAvailableBytes
 * @property {number} loopDelayP99Ms
 * @property {number} cpu CPU time ÷ wall time since the last reading
 * @property {Record<string, { p95Ms: number, baselineMs: number, count: number }>} latency
 */

/**
 * @typedef {object} AdaptiveOptions
 * @property {typeof measured} [baseline]
 * @property {Partial<Thresholds>} [thresholds]
 * @property {number} [intervalMs] how often pressure is read (default 1000)
 * @property {number} [samplesForLatency] a latency key needs this many observations in a window (default 20)
 * @property {number} [calm] consecutive lower readings before the level goes down (default 3)
 * @property {import("./admission.js").AdmissionOptions} [admission]
 * @property {(level: Level, prev: Level, reasons: string[]) => void} [onChange]
 * @property {NodeJS.ProcessEnv} [env]
 * @property {Partial<typeof native>} [readers] replaces the OS readers (tests)
 */

/** @param {AdaptiveOptions} [opts] */
export function createAdaptive(opts = {}) {
  const env = opts.env ?? process.env;
  const base = opts.baseline ?? measured;
  const t = { ...defaultThresholds, ...opts.thresholds };
  const intervalMs = opts.intervalMs ?? 1000;
  const minSamples = opts.samplesForLatency ?? 20;
  const calm = opts.calm ?? 3;
  const read = { ...native, ...opts.readers };
  const num = (/** @type {string | undefined} */ v) => (v && Number(v) > 0 ? Number(v) : 0);

  const envHard = num(env.POLYVISOR_MEMORY_HARD_BYTES);
  const envSoft = num(env.POLYVISOR_MEMORY_SOFT_BYTES);
  const admission = createAdmission({
    backgroundMax: num(env.POLYVISOR_MAX_CONCURRENCY) || undefined,
    ...opts.admission,
  });

  /** @type {Level} */
  let level = "normal";
  let lower = 0;
  /** @type {string[]} */
  let reasons = [];
  /** @type {Readings | null} */
  let last = null;
  let changes = 0;

  // event-loop delay by timer drift: works in Node and in Bun (whose monitorEventLoopDelay/ELU are absent)
  /** @type {number[]} */
  let lags = [];
  /** @type {NodeJS.Timeout | null} */
  let lagTimer = null;
  const LAG_TICK = 20;
  let expected = 0;
  function lagTick() {
    const now = performance.now();
    if (expected) lags.push(Math.max(0, now - expected));
    if (lags.length > 4096) lags = lags.slice(-2048);
    expected = now + LAG_TICK;
    lagTimer = setTimeout(lagTick, LAG_TICK);
    lagTimer.unref?.();
  }

  /** @type {Map<string, number[]>} */
  let latency = new Map();
  let cpuAt = { wall: performance.now(), cpu: read.cpuMicros?.() ?? 0 };
  /** @type {NodeJS.Timeout | null} */
  let timer = null;

  /** @param {number[]} values @param {number} p */
  const percentile = (values, p) => {
    if (!values.length) return 0;
    const s = [...values].sort((a, b) => a - b);
    return s[Math.min(s.length - 1, Math.ceil(p * s.length) - 1)];
  };
  const mb = (/** @type {number} */ b) => `${Math.round(b / 1048576)} MB`;

  /** @returns {Readings} */
  function measure() {
    const proc = read.processMemory?.() ?? { privateBytes: 0, workingSetBytes: 0 };
    const j = read.job?.() ?? null;
    const sys = read.systemMemory?.() ?? { totalBytes: 0, availableBytes: 0, loadPercent: 0 };
    const limit = j?.memoryLimitBytes || envHard;
    const used = j && j.memoryBytes > 0 ? j.memoryBytes : proc.privateBytes;
    const now = performance.now();
    const cpuNow = read.cpuMicros?.() ?? 0;
    const cpu = now > cpuAt.wall ? (cpuNow - cpuAt.cpu) / ((now - cpuAt.wall) * 1000) : 0;
    cpuAt = { wall: now, cpu: cpuNow };
    const loopDelayP99Ms = percentile(lags, 0.99);
    lags = [];
    /** @type {Readings["latency"]} */
    const lat = {};
    for (const [key, values] of latency) {
      const b = base.latency[key];
      if (b && values.length >= minSamples) lat[key] = { p95Ms: percentile(values, 0.95), baselineMs: b.p95Ms, count: values.length };
    }
    latency = new Map();
    return {
      memoryBytes: used,
      memoryLimitBytes: limit,
      memorySoftBytes: envSoft || (limit ? limit * t.memorySoftDefault : 0),
      systemLoadPercent: sys.loadPercent,
      systemAvailableBytes: sys.availableBytes,
      loopDelayP99Ms: Math.round(loopDelayP99Ms * 1000) / 1000,
      cpu: Math.round(cpu * 1000) / 1000,
      latency: lat,
    };
  }

  /**
   * the level each signal asks for, and why
   * @param {Readings} r
   * @returns {{ level: Level, reasons: string[] }}
   */
  function evaluate(r) {
    /** @type {Level} */
    let worst = "normal";
    /** @type {string[]} */
    const why = [];
    /** @param {Level} l @param {string} reason */
    const raise = (l, reason) => {
      if (l === "normal") return;
      why.push(`${l}: ${reason}`);
      if (rank(l) > rank(worst)) worst = l;
    };
    if (r.memoryLimitBytes) {
      if (r.memoryBytes >= r.memoryLimitBytes * t.memoryCritical) raise("critical", `memory ${mb(r.memoryBytes)} ≥ ${Math.round(t.memoryCritical * 100)}% of ${mb(r.memoryLimitBytes)}`);
      else if (r.memorySoftBytes && r.memoryBytes >= r.memorySoftBytes) raise("elevated", `memory ${mb(r.memoryBytes)} ≥ soft limit ${mb(r.memorySoftBytes)}`);
    }
    if (r.systemLoadPercent >= t.systemCritical) raise("critical", `system memory load ${r.systemLoadPercent}%`);
    else if (r.systemLoadPercent >= t.systemElevated) raise("elevated", `system memory load ${r.systemLoadPercent}%`);
    const loop = r.loopDelayP99Ms / base.loopDelayP99Ms;
    if (loop >= t.loopCritical) raise("critical", `event-loop p99 ${r.loopDelayP99Ms} ms (${Math.round(loop)}× baseline)`);
    else if (loop >= t.loopElevated) raise("elevated", `event-loop p99 ${r.loopDelayP99Ms} ms (${Math.round(loop)}× baseline)`);
    if (r.cpu >= t.cpuCritical) raise("critical", `cpu ${Math.round(r.cpu * 100)}%`);
    else if (r.cpu >= t.cpuElevated) raise("elevated", `cpu ${Math.round(r.cpu * 100)}%`);
    for (const [key, l] of Object.entries(r.latency)) {
      const x = l.p95Ms / l.baselineMs;
      if (x >= t.latencyCritical) raise("critical", `${key} p95 ${Math.round(l.p95Ms)} ms (${Math.round(x)}× baseline)`);
      else if (x >= t.latencyElevated) raise("elevated", `${key} p95 ${Math.round(l.p95Ms)} ms (${Math.round(x)}× baseline)`);
    }
    return { level: worst, reasons: why };
  }

  function tick() {
    last = measure();
    const next = evaluate(last);
    const prev = level;
    if (rank(next.level) > rank(level)) {
      level = next.level; // up at once
      lower = 0;
    } else if (rank(next.level) < rank(level)) {
      if (++lower >= calm) {
        level = LEVELS[rank(level) - 1]; // down one step at a time
        lower = 0;
      }
    } else lower = 0;
    reasons = next.reasons;
    admission.adjust(level);
    if (level !== prev) {
      changes++;
      try {
        opts.onChange?.(level, prev, reasons);
      } catch {
        // an observer's error is not the sampler's
      }
    }
  }

  /** @param {any} req @returns {WorkClass} */
  function defaultClassify(req) {
    const path = String(req.path ?? req.url ?? "");
    if (/^\/(health|ready|live|status)\b/.test(path) || /\/(login|auth)\b/.test(path)) return "critical";
    if (/\/(import|imports|export|exports|report|reports|batch|sync)\b/.test(path)) return "background";
    return "interactive";
  }

  const self = {
    start() {
      if (timer) return self;
      expected = 0;
      lagTick();
      cpuAt = { wall: performance.now(), cpu: read.cpuMicros?.() ?? 0 };
      timer = setInterval(tick, intervalMs);
      timer.unref?.();
      return self;
    },
    stop() {
      if (timer) clearInterval(timer);
      if (lagTimer) clearTimeout(lagTimer);
      timer = lagTimer = null;
    },
    /** reads and evaluates now (also what the interval does) */
    tick,
    /** @returns {{ level: Level, reasons: string[], readings: Readings | null }} */
    pressure: () => ({ level, reasons, readings: last }),
    /** @param {string} key e.g. "GET /products/snapshot", "db select produtos" @param {number} ms */
    observe(key, ms) {
      if (!base.latency[key]) return;
      let values = latency.get(key);
      if (!values) latency.set(key, (values = []));
      if (values.length < 4096) values.push(ms);
    },
    /** @param {WorkClass} [cls] */
    admit: (cls) => admission.acquire(cls),
    /**
     * @template T
     * @param {WorkClass} cls
     * @param {() => T | Promise<T>} fn
     * @returns {Promise<T>}
     */
    async run(cls, fn) {
      const release = await admission.acquire(cls);
      try {
        return await fn();
      } finally {
        release();
      }
    },
    /**
     * Express/connect middleware: admission by class, 503 + Retry-After when shed, latency observed per
     * "METHOD /route" (the Express route pattern when there is one).
     * @param {{ classify?: (req: any) => WorkClass }} [o]
     */
    middleware(o = {}) {
      const classify = o.classify ?? defaultClassify;
      return (/** @type {any} */ req, /** @type {any} */ res, /** @type {(err?: unknown) => void} */ next) => {
        const t0 = performance.now();
        admission.acquire(classify(req)).then(
          (release) => {
            let done = false;
            const finish = () => {
              if (done) return;
              done = true;
              release();
              const route = req.route?.path != null ? `${req.baseUrl ?? ""}${req.route.path}` : (req.baseUrl ?? "") + (req.path ?? "");
              self.observe(`${req.method} ${route}`, performance.now() - t0);
            };
            res.once("finish", finish);
            res.once("close", finish);
            next();
          },
          (err) => {
            if (!(err instanceof Shed)) return next(err);
            res.statusCode = 503;
            res.setHeader("Retry-After", String(err.retryAfterSeconds));
            res.setHeader("Content-Type", "application/json");
            res.end(JSON.stringify({ error: "overloaded", detail: err.message, retryAfterSeconds: err.retryAfterSeconds }));
          },
        );
      };
    },
    /**
     * one adaptive.pressure sample with the latest readings and the admission state (for host telemetry's
     * extraSamples: it lands in the TCC dashboard next to process.memory and event-loop.delay)
     */
    samples() {
      if (!last) return [];
      const a = admission.snapshot();
      return [
        {
          type: "adaptive.pressure",
          category: "runtime",
          level,
          reasons,
          backend: native.backend,
          supervised: env.POLYVISOR === "1",
          memoryBytes: last.memoryBytes,
          memoryLimitBytes: last.memoryLimitBytes,
          memorySoftBytes: last.memorySoftBytes,
          systemLoadPercent: last.systemLoadPercent,
          loopDelayP99Ms: last.loopDelayP99Ms,
          cpu: last.cpu,
          latency: last.latency,
          concurrencyLimit: a.limit,
          inflight: a.inflight,
          queued: a.queued,
          admitted: a.admitted,
          shed: a.shed,
          levelChanges: changes,
        },
      ];
    },
    admission,
    get level() {
      return level;
    },
  };
  return self;
}

/** @typedef {ReturnType<typeof createAdaptive>} Adaptive */
