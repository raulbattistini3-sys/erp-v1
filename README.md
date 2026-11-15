# erp-v1: running the ERP with its observability (internal notes)

What lives here:

| folder | what | port |
| --- | --- | --- |
| `launcher` | the native launcher (Rust): `cargo xtask dist` builds both applications into `bin/`; `bin/sample-erp.exe` starts the ERP and the TCC ([launcher/README.md](launcher/README.md)) | as below |
| `secrets` | dev/compose TLS certificates and tokens (`bun generate.ts`); moved up from the ERP repo | — |
| `sample-ERP/sample-ERP` | the ERP (git): network gateway (Go), API (bun/Express), web, importer, vault, billing | gateway `:8080`, API `:10000`, web `:5173` |
| `tcc-v2/tcc-v1/demo-monorepo-workspace` | the TCC telemetry server + dashboard (thesis). **Copy** of `C:\workarea\experiments\tcc-v2\tcc-v1`, which is where TCC work happens; sync before running | server `:8787`, dashboard `:5175` |
| `cache` | `erp-cache`, the Go read cache in front of the API and the TCC server ([cache/README.md](cache/README.md)) | erp `:10080`, tcc `:8788`, control `:7071` |

How they relate:

```
                go run ./network/cmd/network dev            (one terminal, Job Object on Windows)
                ├── network gateway :8080  ──► api :10000 ──► importer, vault, billing (API_CHILDREN)
                ├── web  :5173
                ├── tcc  :8787   ◄── POST /telemetry/host every 10 s (API, erp-cache)   [sidecar]
                └── cache :10080/:8788                                                [sidecar, optional]
```

- **The API orchestrates the application** (its children, readiness). The gateway only checks the API.
- **The TCC server is a sibling, not a dependency.** If it is down or missing, the ERP runs exactly the
  same; host envelopes wait in a bounded queue (1 h), app events (product / Empresa search timings) in a
  small one that drops the oldest. Nothing is written to disk any more (`TELEMETRY_DIR` is gone).
- Sidecars ending never stop the stack; `api` or `web` ending stops everything.

## First time

```sh
# TCC server: deps, database, token
cd tcc-v2/tcc-v1/demo-monorepo-workspace
pnpm install
cp apps/server/.env.example apps/server/.env     # DATABASE_URL, DATABASE_MIGRATION_URL, (DATABASE_CA_CERT), TCC_TOKEN
pnpm db:migrate

# ERP: as in sample-ERP/sample-ERP/README.md (bun install in api/ and web/, api/.env, database)
```

Put the **same `TCC_TOKEN`** in the TCC server's `apps/server/.env` and in the ERP API's environment
(`api/.env` or the shell). Without it the TCC server generates a random token at boot and rejects the
API's envelopes (the ERP still runs; its telemetry is dropped).

## Every day

```sh
cd sample-ERP/sample-ERP
go run ./network/cmd/network dev
```

That starts, in one terminal with `[name]` prefixes: `network`, `tcc` (if the TCC workspace is found),
`cache` (if `cache/cache.json` exists), `api` (+ importer, vault, billing), `web`. Ctrl+C stops all.

| flag / env | effect |
| --- | --- |
| `--no-tcc`, `--no-cache`, `--no-web`, `--no-api` | skip one |
| `TCC_DIR=...\demo-monorepo-workspace` | use that TCC workspace (e.g. the newest one, `C:\workarea\experiments\tcc-v2\tcc-v1\demo-monorepo-workspace`) instead of searching for `tcc-v2/tcc-v1/...` above the repo |
| `TCC_DEV_SCRIPT=dev` | also start the TCC dashboard (default `dev:server`: server only) |
| `TCC_ADDR` | where the TCC server listens (default `127.0.0.1:8787`); the API gets `TCC_TELEMETRY_URL=http://$TCC_ADDR/telemetry/host` |
| `TCC_TELEMETRY_URL` | already set: kept as is (point the API at another TCC server) |
| `CACHE_DIR`, `CACHE_CONFIG` | where erp-cache is / its config |

Starting things by hand instead (separate terminals):

```sh
cd tcc-v2/tcc-v1/demo-monorepo-workspace && pnpm dev          # TCC server + dashboard
cd cache && go run ./cmd/erp-cache                            # optional
cd sample-ERP/sample-ERP && TCC_TELEMETRY_URL=http://127.0.0.1:8787/telemetry/host go run ./network/cmd/network dev --no-tcc --no-cache
```

## Looking at it

- **TCC dashboard** `http://localhost:5175` → Telemetry: per-boot memory, heap spaces, GC, event-loop
  delay/utilization, requests and queries per route, lifecycle (started / ready / degraded / recovered /
  restarted), and the `erp-cache` source's hit/miss/eviction numbers.
- **TCC API**: `GET :8787/telemetry/host/boots`, `/latest`, `/series`, `/lifecycle` (Bearer `TCC_TOKEN`).
- **Per response**: the API sends `Server-Timing: app;dur=…, db;dur=…` (and `Timing-Allow-Origin` for
  the web origins), so DevTools → Network → Timing shows where a request spent its time; through the
  cache, also `X-Cache` and `cache;dur=…`.
- **Browser side**: the TCC WebExtension (`tcc-v1/extension`) records navigation/resource timing,
  FCP/LCP/CLS/INP, long animation frames and interactions of the ERP pages and uploads them to `/ingest`.
- **Search timings**: the web app's product and Empresa searches (`engine-ready`, `request`, with
  `cold: true` on the first load per endpoint) go via the API's `/telemetry` to the TCC's
  `POST /telemetry/events`.
- **Cache**: `GET :7071/stats`, `GET :7071/health`; at startup it logs (and sends as a `cold-start`
  sample) each database's connect / `SELECT 1` / first read of every table, and the first-vs-second time of
  each `warmup` path ([cache/README.md](cache/README.md#cold-start)).

## The packaged build (bin/)

```powershell
cd launcher
cargo xtask dist            # ERP services + services/tcc/tcc-v2.exe into ..\bin
..\bin\sample-erp.exe       # ERP on :8080, TCC dashboard on :8787
```

The TCC is built from `tcc-v2/tcc-v1` here, so sync it from `C:\workarea\experiments\tcc-v2\tcc-v1` first.

## Windows counters and ETW (native addon)

The TCC server can add what Windows knows about it to its own telemetry: `win.process` (working set, private
bytes, CPU %, handles, GDI/USER objects, I/O), `win.job` (the job object network dev puts the stack in:
processes alive/ended, CPU %, I/O, peak memory), `win.system` (memory load, commit, CPU %), and with `WIN_ETW=1`
`win.etw` (Kernel-Process / Kernel-Network events per window). Once:

```powershell
cd C:\workarea\experiments\tcc-v2\tcc-v1\demo-monorepo-workspace
pnpm build:native        # MinGW g++ or MSVC cl, no node-gyp
```

From then on it's on (`WIN_TELEMETRY=auto`). ETW needs the terminal that runs `network dev` elevated, or your
user in "Performance Log Users"; without it the server prints a warning and keeps the rest. Details:
`tcc-v1/demo-monorepo-workspace/packages/win-telemetry/README.md`. The ERP API doesn't load the addon yet (it
takes it from the TCC side once it's settled, like the rest).

## Heap snapshots

Never automatic (they pause the process and contain whatever strings it holds). On your own machine:
`writeHeapSnapshot(dir)` from `pkg/telemetry/host.js` (ERP) or `@tcc/host-telemetry` (TCC), then open
the `.heapsnapshot` in Chrome DevTools → Memory.

## Where changes go

TCC features (envelope, observers, dashboard) are made in `C:\workarea\experiments\tcc-v2\tcc-v1` first;
the ERP takes them afterwards (`pkg/telemetry/host.js` is the JS port of `@tcc/host-telemetry`), so the
ERP's `.exe` build stays on a tested version.
