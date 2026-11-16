import { test } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { createAdaptive, measured } from "../src/runtime/index.js";

const MB = 1024 * 1024;

/** fake OS readers: the test sets what the next reading returns */
function fakeOS() {
  const os = { privateBytes: 100 * MB, jobBytes: 0, limit: 0, load: 50, cpu: 0 };
  const readers = {
    processMemory: () => ({ privateBytes: os.privateBytes, workingSetBytes: os.privateBytes }),
    job: () => (os.limit ? { kind: "job", memoryLimitBytes: os.limit, memoryBytes: os.jobBytes, peakMemoryBytes: 0, processes: 2 } : null),
    systemMemory: () => ({ totalBytes: 8192 * MB, availableBytes: 4096 * MB, loadPercent: os.load }),
    cpuMicros: () => os.cpu,
  };
  return { os, readers };
}

test("memory against the job limit: soft → elevated, 95% → critical, down one step after calm readings", () => {
  const { os, readers } = fakeOS();
  const changes = [];
  const a = createAdaptive({ readers, env: {}, calm: 2, onChange: (l, p) => changes.push(`${p}→${l}`) });
  os.limit = 512 * MB;
  os.jobBytes = 200 * MB;
  a.tick();
  assert.equal(a.level, "normal");
  os.jobBytes = 420 * MB; // ≥ 80% default soft
  a.tick();
  assert.equal(a.level, "elevated");
  assert.match(a.pressure().reasons[0], /soft limit 410 MB/);
  os.jobBytes = 500 * MB; // ≥ 95%
  a.tick();
  assert.equal(a.level, "critical");
  os.jobBytes = 100 * MB;
  a.tick();
  assert.equal(a.level, "critical", "one calm reading is not enough");
  a.tick();
  assert.equal(a.level, "elevated");
  a.tick();
  a.tick();
  assert.equal(a.level, "normal");
  assert.deepEqual(changes, ["normal→elevated", "elevated→critical", "critical→elevated", "elevated→normal"]);
});

test("the env limits apply when there is no job (and the 7880 MB 'heap limit' never counts)", () => {
  const { os, readers } = fakeOS();
  const a = createAdaptive({ readers, env: { POLYVISOR_MEMORY_HARD_BYTES: String(256 * MB), POLYVISOR_MEMORY_SOFT_BYTES: String(128 * MB) } });
  os.privateBytes = 150 * MB;
  a.tick();
  assert.equal(a.level, "elevated");
  assert.equal(a.pressure().readings?.memoryLimitBytes, 256 * MB);
  const b = createAdaptive({ readers: fakeOS().readers, env: {} });
  b.tick();
  assert.equal(b.pressure().readings?.memoryLimitBytes, 0, "no limit known: memory alone never raises pressure");
  assert.ok(measured.machineMemoryBytes > 7000 * MB);
});

test("system memory load above what was measured as normal (90%)", () => {
  const { os, readers } = fakeOS();
  const a = createAdaptive({ readers, env: {} });
  os.load = 90;
  a.tick();
  assert.equal(a.level, "normal", "90% was the measured normal");
  os.load = 94;
  a.tick();
  assert.equal(a.level, "elevated");
  os.load = 98;
  a.tick();
  assert.equal(a.level, "critical");
});

test("route latency against its measured baseline (snapshot p95 50.4 ms)", () => {
  const { readers } = fakeOS();
  const a = createAdaptive({ readers, env: {}, samplesForLatency: 5 });
  for (let i = 0; i < 10; i++) a.observe("GET /products/snapshot", 60);
  a.tick();
  assert.equal(a.level, "normal");
  for (let i = 0; i < 10; i++) a.observe("GET /products/snapshot", 200); // ~4× baseline
  a.observe("GET /unknown", 1e6); // no baseline: ignored
  a.tick();
  assert.equal(a.level, "elevated");
  assert.match(a.pressure().reasons.join(), /GET \/products\/snapshot p95 200 ms \(4× baseline\)/);
});

test("CPU time stands in for event-loop utilization (0 under Bun)", () => {
  const { os, readers } = fakeOS();
  const a = createAdaptive({ readers, env: {} });
  a.start();
  a.stop();
  const t0 = performance.now();
  while (performance.now() - t0 < 30);
  os.cpu += 1e9; // way more CPU than wall time
  a.tick();
  assert.equal(a.level, "critical");
  assert.match(a.pressure().reasons.join(), /cpu/);
});

test("the middleware sheds with 503 + Retry-After and observes routes", async () => {
  const { os, readers } = fakeOS();
  const a = createAdaptive({ readers, env: {}, admission: { max: 1, min: 1, queueLimit: 0 } });
  const mw = a.middleware();
  const res = () => Object.assign(new EventEmitter(), { statusCode: 200, headers: {}, body: "", setHeader(k, v) { this.headers[k] = v; }, end(b) { this.body = b; } });
  const first = res();
  let nexted = 0;
  mw({ method: "GET", path: "/products/snapshot", baseUrl: "" }, first, () => nexted++);
  await new Promise((r) => setImmediate(r));
  assert.equal(nexted, 1);
  const second = res();
  mw({ method: "GET", path: "/products/snapshot", baseUrl: "" }, second, () => nexted++);
  await new Promise((r) => setImmediate(r));
  assert.equal(second.statusCode, 503);
  assert.equal(second.headers["Retry-After"], "1");
  assert.match(second.body, /overloaded/);
  const health = res();
  mw({ method: "GET", path: "/health", baseUrl: "" }, health, () => nexted++);
  await new Promise((r) => setImmediate(r));
  assert.equal(nexted, 2, "health checks are critical: admitted while saturated");
  first.emit("finish");
  os.load = 0;
  a.tick();
  const [s] = a.samples();
  assert.equal(s.type, "adaptive.pressure");
  assert.equal(s.category, "runtime");
  assert.equal(s.shed, 1);
});
