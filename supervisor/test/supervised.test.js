// End to end: the real polyvisor binary runs a Node service that loads the runtime (and on Windows the
// addon), and the service sees the limit it was given from inside its own job/cgroup.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, writeFileSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { binaryPath } from "../src/index.js";

let exe = null;
try {
  exe = binaryPath();
} catch {
  // not built: skipped
}

test("a supervised service reads its own hard limit through the runtime", { skip: !exe && "polyvisor binary not built (npm run build:bin -- --host)", timeout: 60000 }, () => {
  const dir = mkdtempSync(join(tmpdir(), "polyvisor-e2e-"));
  const runtime = new URL("../src/runtime/index.js", import.meta.url).href;
  const out = join(dir, "reading.json");
  writeFileSync(
    join(dir, "service.mjs"),
    `const { createAdaptive, native } = await import(${JSON.stringify(runtime)});
     const a = createAdaptive();
     a.tick();
     const fs = await import("node:fs");
     fs.writeFileSync(${JSON.stringify(out)}, JSON.stringify({ backend: native.backend, job: native.job(), readings: a.pressure().readings, env: process.env.POLYVISOR_MEMORY_HARD_BYTES }));
     console.log("reading written");`,
  );
  writeFileSync(
    join(dir, "polyvisor.yaml"),
    `application:
  name: e2e-${process.pid}
  services:
    svc:
      command: ${JSON.stringify(process.execPath)}
      args: [service.mjs]
      required: true
      memory: { softLimit: 192MB, hardLimit: 256MB }
      restart: { policy: never }
`,
  );
  // a required service that exits 0 with policy never: the application stops with it
  const r = spawnSync(exe, ["run", "-c", join(dir, "polyvisor.yaml")], { cwd: dir, encoding: "utf8", timeout: 50000 });
  assert.match(r.stdout + r.stderr, /reading written/, r.stdout + r.stderr);
  assert.ok(existsSync(out));
  const got = JSON.parse(readFileSync(out, "utf8"));
  assert.equal(got.env, String(256 * 1024 * 1024));
  assert.equal(got.readings.memoryLimitBytes, 256 * 1024 * 1024);
  if (process.platform === "win32" && got.backend === "addon") {
    assert.equal(got.job.kind, "job");
    assert.equal(got.job.memoryLimitBytes, 256 * 1024 * 1024, "the addon found the service's own job and its limit");
    assert.ok(got.job.memoryBytes > 0);
  }
});
