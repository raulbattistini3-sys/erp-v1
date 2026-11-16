import { test } from "node:test";
import assert from "node:assert/strict";
import { createAdmission, Shed } from "../src/runtime/admission.js";

test("interactive work queues past the limit and runs when capacity frees", async () => {
  const a = createAdmission({ max: 2, min: 1, queueTimeoutMs: 1000 });
  const r1 = await a.acquire("interactive");
  const r2 = await a.acquire("interactive");
  let third = false;
  const p = a.acquire("interactive").then((r) => {
    third = true;
    return r;
  });
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(third, false);
  assert.equal(a.snapshot().queued.interactive, 1);
  r1();
  (await p)();
  assert.equal(third, true);
  r2();
  assert.deepEqual(a.snapshot().inflight, { critical: 0, interactive: 0, background: 0 });
});

test("critical work is always admitted", async () => {
  const a = createAdmission({ max: 1, min: 1 });
  await a.acquire("interactive");
  a.adjust("critical");
  const release = await a.acquire("critical");
  release();
});

test("background is held when elevated and refused when critical", async () => {
  const a = createAdmission({ max: 8, min: 2, queueTimeoutMs: 1000 });
  a.adjust("elevated");
  let ran = false;
  const held = a.acquire("background").then(
    () => (ran = true),
    (err) => err,
  );
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(ran, false);
  a.adjust("critical");
  assert.ok((await held) instanceof Shed, "queued background work is refused at critical");
  await assert.rejects(a.acquire("background"), Shed);
  for (let i = 0; i < 10; i++) a.adjust("normal");
  (await a.acquire("background"))();
});

test("AIMD: the limit halves on critical, shrinks on elevated, grows by one when normal", () => {
  const a = createAdmission({ max: 64, min: 2 });
  a.adjust("critical");
  assert.equal(a.limit, 32);
  a.adjust("elevated");
  assert.equal(a.limit, 28);
  for (let i = 0; i < 5; i++) a.adjust("critical");
  assert.equal(a.limit, 2);
  a.adjust("normal");
  assert.equal(a.limit, 3);
});

test("a full queue sheds and a queued unit times out", async () => {
  const a = createAdmission({ max: 1, min: 1, queueLimit: 1, queueTimeoutMs: 30 });
  await a.acquire("interactive");
  const waiting = a.acquire("interactive");
  await assert.rejects(a.acquire("interactive"), /units waiting/);
  await assert.rejects(waiting, /waited 30 ms/);
  assert.equal(a.snapshot().timedOut, 1);
});

test("background uses at most its share (and POLYVISOR_MAX_CONCURRENCY)", async () => {
  const a = createAdmission({ max: 10, backgroundMax: 2, queueTimeoutMs: 50 });
  await a.acquire("background");
  await a.acquire("background");
  await assert.rejects(a.acquire("background"), Shed); // 3rd waits 6×50 ms then times out... or is shed
  (await a.acquire("interactive"))();
});
