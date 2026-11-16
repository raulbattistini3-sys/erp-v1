// What "normal" looked like, measured on the TCC dashboard (Telemetria → Servidor) of sample-erp running on
// an 8 GB Windows machine (data/measurements.pdf, the packaged Bun executables under the launcher):
//
//   event-loop.delay p99 ............ 3.1 ms         (API, idle-to-moderate load)
//   db.query "select produtos" p95 .. 39.294 ms
//   GET /products/snapshot p95 ...... 50.377 ms
//   GET /clientes/ p95 .............. 2.779 ms
//   win.system memory load .......... 90 % of 7 880.6 MB
//   process.heap "limite" ........... 7 880.6 MB     (what Bun reports as the heap limit: the machine's RAM,
//                                                     i.e. what may be allocated, not what is used; the real
//                                                     ceiling is the job object's limit)
//   event-loop.utilization .......... 0 %            (Bun doesn't implement it: CPU time is used instead)
//
// The thresholds are multiples of these, so a service that is "8x slower than it was measured" is under
// pressure whatever its absolute numbers are; pass your own baseline to createAdaptive() to recalibrate.

/** @typedef {{ p95Ms: number }} LatencyBaseline */

export const measured = Object.freeze({
  source: "data/measurements.pdf",
  machineMemoryBytes: 7880.6 * 1024 * 1024,
  systemLoadPercent: 90,
  loopDelayP99Ms: 3.1,
  /** @type {Readonly<Record<string, LatencyBaseline>>} */
  latency: Object.freeze({
    "GET /products/snapshot": { p95Ms: 50.377 },
    "GET /clientes/": { p95Ms: 2.779 },
    "db select produtos": { p95Ms: 39.294 },
  }),
});

/**
 * @typedef {object} Thresholds
 * @property {number} loopElevated   event-loop p99 ÷ baseline that means elevated (8 → ~25 ms)
 * @property {number} loopCritical   … critical (32 → ~100 ms)
 * @property {number} latencyElevated  a route's p95 ÷ its baseline that means elevated
 * @property {number} latencyCritical
 * @property {number} memoryCritical   fraction of the hard limit that is critical (soft limit is elevated)
 * @property {number} memorySoftDefault fraction of the hard limit used as the soft limit when none is set
 * @property {number} systemElevated   system memory load % that is elevated: the measured 90 % was normal
 *                                     for that machine, so a bit above it
 * @property {number} systemCritical
 * @property {number} cpuElevated      CPU time ÷ wall time of the process (1 = one core busy)
 * @property {number} cpuCritical
 */

/** @type {Readonly<Thresholds>} */
export const thresholds = Object.freeze({
  loopElevated: 8,
  loopCritical: 32,
  latencyElevated: 3,
  latencyCritical: 10,
  memoryCritical: 0.95,
  memorySoftDefault: 0.8,
  systemElevated: 93,
  systemCritical: 97,
  cpuElevated: 0.85,
  cpuCritical: 0.98,
});
