// Admission control: how much work a service takes on, by class, given the pressure it's under.
//
//   critical     always admitted (health checks, the supervisor's own probes, auth)
//   interactive  a user is waiting: admitted up to the concurrency limit, then queued (bounded, with a timeout)
//   background   imports, reports, queue consumers: share of the limit when normal, held when elevated,
//                rejected when critical
//
// The limit moves AIMD-style with each pressure reading: +1 per normal reading (up to max), ×0.9 per
// elevated one and ×0.5 per critical one (down to min). The queue drains interactive before background.

/** @typedef {"critical" | "interactive" | "background"} WorkClass */
/** @typedef {"normal" | "elevated" | "critical"} Level */

export class Shed extends Error {
  /** @param {string} message @param {number} retryAfterSeconds */
  constructor(message, retryAfterSeconds) {
    super(message);
    this.name = "Shed";
    this.status = 503;
    this.retryAfterSeconds = retryAfterSeconds;
  }
}

/** @type {readonly WorkClass[]} */
export const CLASSES = ["critical", "interactive", "background"];

/**
 * @typedef {object} AdmissionOptions
 * @property {number} [max] most concurrent interactive+background units (default 64)
 * @property {number} [min] the limit never goes below this (default 2)
 * @property {number} [backgroundMax] most concurrent background units when normal (POLYVISOR_MAX_CONCURRENCY)
 * @property {number} [backgroundShare] fraction of the limit background may use when normal (default 0.5)
 * @property {number} [queueLimit] waiting units beyond which new ones are shed (default 256)
 * @property {number} [queueTimeoutMs] a queued unit is shed after this (default 5000)
 */

/** @param {AdmissionOptions} [opts] */
export function createAdmission(opts = {}) {
  const max = Math.max(1, opts.max ?? 64);
  const min = Math.min(max, Math.max(1, opts.min ?? 2));
  const backgroundMax = opts.backgroundMax ?? Infinity;
  const backgroundShare = opts.backgroundShare ?? 0.5;
  const queueLimit = opts.queueLimit ?? 256;
  const queueTimeoutMs = opts.queueTimeoutMs ?? 5000;

  let limit = max;
  /** @type {Level} */
  let level = "normal";
  const inflight = { critical: 0, interactive: 0, background: 0 };
  const stats = { admitted: 0, waited: 0, shed: 0, timedOut: 0 };
  /** @type {Record<"interactive" | "background", Array<{ grant: () => void, refuse: (err: Error) => void, timer: NodeJS.Timeout }>>} */
  const queues = { interactive: [], background: [] };

  /** @param {WorkClass} cls */
  function cap(cls) {
    const l = Math.floor(limit);
    if (cls === "critical") return Infinity;
    if (cls === "interactive") return l;
    if (level !== "normal") return 0;
    return Math.min(backgroundMax, Math.max(1, Math.floor(l * backgroundShare)));
  }

  /** @param {WorkClass} cls */
  function fits(cls) {
    if (cls === "critical") return true;
    const shared = inflight.interactive + inflight.background;
    return shared < Math.floor(limit) && inflight[cls] < cap(cls);
  }

  /** @param {WorkClass} cls @returns {() => void} */
  function grant(cls) {
    inflight[cls]++;
    stats.admitted++;
    let released = false;
    return () => {
      if (released) return;
      released = true;
      inflight[cls]--;
      drain();
    };
  }

  function drain() {
    for (const cls of /** @type {const} */ (["interactive", "background"])) {
      const q = queues[cls];
      while (q.length && fits(cls)) {
        const next = /** @type {(typeof q)[number]} */ (q.shift());
        clearTimeout(next.timer);
        next.grant();
      }
    }
  }

  const retryAfter = () => (level === "critical" ? 10 : level === "elevated" ? 5 : 1);

  return {
    /**
     * resolves with a release function once the unit may run; rejects with Shed when it may not
     * @param {WorkClass} [cls]
     * @returns {Promise<() => void>}
     */
    acquire(cls = "interactive") {
      if (!CLASSES.includes(cls)) cls = "interactive";
      if (fits(cls) && (cls === "critical" || queues[cls].length === 0)) return Promise.resolve(grant(cls));
      if (cls === "background" && level === "critical") {
        stats.shed++;
        return Promise.reject(new Shed("background work refused: critical pressure", retryAfter()));
      }
      const q = queues[/** @type {"interactive" | "background"} */ (cls)];
      if (queues.interactive.length + queues.background.length >= queueLimit) {
        stats.shed++;
        return Promise.reject(new Shed(`overloaded: ${queueLimit} units waiting`, retryAfter()));
      }
      stats.waited++;
      return new Promise((resolve, reject) => {
        const entry = {
          grant: () => resolve(grant(cls)),
          refuse: reject,
          timer: setTimeout(() => {
            const i = q.indexOf(entry);
            if (i >= 0) q.splice(i, 1);
            stats.shed++;
            stats.timedOut++;
            reject(new Shed(`waited ${queueTimeoutMs} ms for capacity`, retryAfter()));
          }, cls === "background" ? queueTimeoutMs * 6 : queueTimeoutMs),
        };
        entry.timer.unref?.();
        q.push(entry);
      });
    },

    /**
     * one pressure reading: moves the limit and wakes or sheds waiting units
     * @param {Level} next
     */
    adjust(next) {
      level = next;
      if (next === "normal") limit = Math.min(max, limit + 1);
      else if (next === "elevated") limit = Math.max(min, limit * 0.9);
      else limit = Math.max(min, limit * 0.5);
      if (next === "critical") {
        for (const entry of queues.background.splice(0)) {
          clearTimeout(entry.timer);
          stats.shed++;
          entry.refuse(new Shed("background work refused: critical pressure", retryAfter()));
        }
      }
      drain();
    },

    get limit() {
      return Math.floor(limit);
    },
    get level() {
      return level;
    },
    snapshot() {
      return {
        level,
        limit: Math.floor(limit),
        inflight: { ...inflight },
        queued: { interactive: queues.interactive.length, background: queues.background.length },
        ...stats,
      };
    },
  };
}

/** @typedef {ReturnType<typeof createAdmission>} Admission */
