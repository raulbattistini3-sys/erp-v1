# polyvisor

A supervisor for **local applications that combine runtimes** (Node, Bun, Java, Go, native executables) and
need resource-aware workload management. It is not another PM2: the supervisor is a native Go executable,
every service lives in one process tree under OS resource controls, and services get a small runtime that
turns those limits into admission decisions.

- **One process tree.** Windows: an application job object (kill-on-close) plus a named job per service.
  Linux: cgroup v2 (`polyvisor-<app>/<service>-<n>`). Elsewhere: process groups.
- **Whole-footprint budgets.** `hardLimit` caps the service's process tree (heap, metaspace, native, child
  processes), not the V8/JVM heap flag. `memoryBudget` caps all services together.
- **Limited before the first instruction.** Windows services start suspended, join their jobs, then resume,
  so a child a service forks at start-up is inside the limit too.
- **Dependency-aware startup.** `dependsOn` waits for readiness (`tcp`, `http`, `delay`); shutdown runs in
  reverse order with a grace period per service.
- **Memory-pressure admission.** Optional services wait while the machine is short of memory or the
  budget is spent; soft limits raise `elevated`/`critical` pressure events.
- **Restart policies.** `always | on-failure | never`, exponential backoff, `maxRestarts` per `window`. A
  required service that gives up stops the application with exit code 1.
- **Events and control.** NDJSON lifecycle events, a token-protected control API on 127.0.0.1, and a CLI.

## Use

```sh
npm install -g polyvisor        # or: npx polyvisor ...
polyvisor run -c polyvisor.yaml
polyvisor status                # from another terminal
polyvisor events --follow
polyvisor restart api
polyvisor shutdown
polyvisor validate -c polyvisor.yaml
```

Config: YAML or JSON, see [examples/polyvisor.yaml](examples/polyvisor.yaml). Sizes use binary units
(`512MB` = 512 MiB); durations are seconds or Go durations (`15s`, `5m`).

## Service-side runtime

Each service gets `POLYVISOR=1`, `POLYVISOR_SERVICE`, `POLYVISOR_MEMORY_SOFT_BYTES`,
`POLYVISOR_MEMORY_HARD_BYTES`, `POLYVISOR_MAX_CONCURRENCY`, `POLYVISOR_JOB` (Windows) or
`POLYVISOR_CGROUP` (Linux) and `POLYVISOR_RUNTIME`. In a Node or Bun service:

```js
import { createAdaptive } from "polyvisor/runtime";   // or: await import(process.env.POLYVISOR_RUNTIME + "/index.js")

const adaptive = createAdaptive();   // reads the job/cgroup, system memory, CPU, loop delay, route latency
adaptive.start();
app.use(adaptive.middleware());       // 503 + Retry-After instead of falling over
await adaptive.run("background", () => rebuildSearchIndex());
```

Pressure is `normal | elevated | critical`, from:

| signal | elevated | critical |
| --- | --- | --- |
| memory vs. the service's own limit (job/cgroup, else env) | ≥ soft limit (default 80%) | ≥ 95% |
| system memory load | > 93% | > 97% |
| event-loop delay p99 (timer drift; works under Bun) | > 8 ms | > 32 ms |
| CPU time / wall time (stands in for ELU, which Bun reports as 0) | > 85% | > 98% |
| route p95 vs. its baseline | > 3× | > 10× |

Baselines come from measurements of a real deployment (`src/runtime/baseline.js`: loop p99 3.1 ms,
`GET /products/snapshot` p95 50.4 ms, system memory normally at 90% of an 8 GB machine). The heap limit a
runtime reports (`7880 MB` there — the machine's RAM, allocated, not used) is never taken as the limit.

Admission keeps three classes: `critical` (always admitted: health, auth), `interactive` (queued past the
limit), `background` (held at `elevated`, refused at `critical`). The concurrency limit is AIMD: +1 per calm
reading, ×0.9 when elevated, ×0.5 when critical.

On Windows the runtime loads `polyvisor.node` (C++, Node-API resolved from the host so the same file loads in
Node and Bun) to read private bytes, its job's limits and usage, and system memory. On Linux it reads
`/proc` and the cgroup files. Without either it falls back to the env limits.

## Build

```sh
npm install
npm run build:bin       # Go cross-builds into bin/<os>-<arch>/ (or: node scripts/build-bin.mjs --host)
npm run build:native    # polyvisor.node (cl.exe or MinGW g++)
npm test                # node:test + go test
```
