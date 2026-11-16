# polivisor

The process supervisor for the erp-v1 stack: dependency-aware, memory-capped, a little like PM2 for a set of
coupled services. Stdlib Go only (no cgo), built by `cargo xtask dist` into `bin/polivisor.exe`.

    launcher (sample-erp.exe)   Postgres, migrations, certs, secrets, web build → writes data/polivisor.json
      └─ polivisor.exe          this
           ├─ tier 0  network · tcc · vault         (siblings, start together)
           ├─ tier 1  api                           (depends on network + vault, after tcc)
           └─ tier 2  importer · billing            (depend on api)

    polivisor run    --config polivisor.json   start and supervise
    polivisor check  --config polivisor.json   validate, print tiers
    polivisor status [--addr 127.0.0.1:9900]   table of state / pid / memory / cap / restarts

What it does per service:

- **ordering**: `depends_on` must be *ready* (TCP probe) first, otherwise the service is skipped; `after` only
  orders, so a failed TCC never blocks the API.
- **containment**: each service gets its own Windows Job Object (`KILL_ON_JOB_CLOSE`, optional
  `JOB_MEMORY` cap in `memory_mb`). Forked workers land in the same job and count against the cap. If the
  polivisor dies, even when it is force-killed, the OS tears down every service.
- **admission**: `min_available_mb` makes a (re)start wait, bounded by the ready timeout, until that much RAM
  is free.
- **restarts**: exponential backoff (`backoff_min` → `backoff_max`). More than `max` crashes within
  `window` means it gives up. A `critical` service giving up stops the whole stack.
- **runtime flags**: `runtime.kind` = `node` adds `--max-old-space-size`, `java` adds
  `-Xmx/-XX:MaxMetaspaceSize/-XX:MaxDirectMemorySize`. Bun-compiled exes (api, tcc-v2) run on
  JavaScriptCore and ignore V8 flags, so for them the job cap is the limit.
- **shutdown**: Ctrl+C → tiers stopped top-down with CTRL_BREAK, `grace` (20 s) per tier, then the job is
  terminated. Note that the JVM treats CTRL_BREAK as a thread dump, so billing usually ends at the grace kill.
- **monitoring**: memory per job is sampled every 5 s, with a warning above 90 % of the cap. `/status` and `/healthz`
  are served on the loopback control port.

Next, deliberately not done yet: the `cache` service (Postgres cold starts via `pg_prewarm`, which only
the ERP database has, not the telemetry one), TCC-driven adaptive placement of API work (main thread vs
a long-lived bounded worker), and terminal commands/configs such as telemetry for a given repo.
