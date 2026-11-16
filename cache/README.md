# erp-cache

A read cache that sits **between the gateway and an upstream** (the ERP API, the TCC telemetry
server) and answers repeated GETs from memory instead of making the upstream do the same database
round trips again. Go port of the idea behind `readcache/` in `order-api-with-mim-fixed`, generalised
to any configured route:

- **The upstream stays the only query owner.** The cache never talks SQL for reads; a miss is
  forwarded and the response is kept. Everything that is not a configured GET passes straight through.
- **Bounded.** Per site: `maxEntries`, `maxBytes`, `maxEntryBytes` (LRU eviction) and a TTL per route.
  No unbounded maps.
- **Invalidated by the database, not by guessing.** Triggers (`erp-cache triggers`) make every
  statement on a watched table `pg_notify(<channel>, <table>)`. The cache `LISTEN`s and drops every
  entry read from that table. A response that was in flight while its table changed is not stored.
- **Never serves what it can't prove is current.** If the LISTEN connection drops, the site forgets
  everything and bypasses (every request goes to the upstream) until it is back.
- **Measured.** Every 10 s a `HostTelemetryEnvelope` goes to the TCC server (`POST /telemetry/host`)
  with a `cache` sample per site (hits, misses, evictions, invalidations, entries, bytes, bypassed,
  feed state, upstream latency) plus `go.runtime` and lifecycle (`degraded` when a feed drops,
  `recovered` when it is back). Responses carry `X-Cache: hit|miss|shared|bypass` and
  `Server-Timing: cache;dur=…`.

```
browser ──► gateway (network :8080) ──► erp-cache :10080 ──► ERP API :10000 ──► Postgres
                                         ▲  LISTEN erp_cache ─────────────────────┘
TCC web ──────────────────────────────► erp-cache :8788  ──► TCC server :8787 ──► Postgres
                                         ▲  LISTEN tcc_cache ─────────────────────┘
```

## Run

```sh
cp cache.example.json cache.json        # adjust ports / routes
export ERP_CACHE_DATABASE_URL=postgres://…:5432/erp      # session connection, NOT a tx pooler
export TCC_CACHE_DATABASE_URL=postgres://…:5432/postgres  # Supabase: session pooler or direct, not :6543
export TCC_TOKEN=…                                         # same token as the TCC server

go run ./cmd/erp-cache triggers -site erp -apply   # once per database (idempotent)
go run ./cmd/erp-cache triggers -site tcc -apply
go run ./cmd/erp-cache                              # or: go build -o erp-cache.exe ./cmd/erp-cache
```

`erp-cache triggers -site erp` without `-apply` prints the SQL instead (to put in a migration).

Then point the client at the cache instead of the upstream, e.g. in the ERP
`NETWORK_ROUTES="/api=http://127.0.0.1:10080,…"` (or whatever registers the API address), and the TCC
web dashboard's API base at `:8788`.

A site whose `databaseUrlEnv` variable is unset only bypasses (it has no way to know when data
changes); a site with no `databaseUrlEnv` at all is TTL-only (logged as a warning).

## Config

`cache.json` (or `-config`, or `$CACHE_CONFIG`) over defaults; unknown keys fail startup;
credentials only via the env var *names* in the file. Env overrides: `CACHE_CONTROL_ADDR`,
`TCC_TELEMETRY_URL`, `CACHE_<SITE>_LISTEN`, `CACHE_<SITE>_UPSTREAM`. `CACHE_DEBUG=1` for debug logs.

| key | meaning |
| --- | --- |
| `routes[].path` | `/products/filter`, `/clientes/empresas/:id` (one segment), `/ingest/*` (prefix). First match wins. |
| `routes[].tables` | tables the response is read from; a change in any of them drops it |
| `routes[].ttl` | upper bound even without changes (default `defaultTtl`, 30 s) |
| `vary` | request headers that are part of the key (hashed). **`Authorization` for anything per user** |
| `flushOn` | tables whose change drops the whole site (e.g. `usuarios`: permissions) |
| `channel` | NOTIFY channel, default `<name>_cache` |
| `warmup` | GET paths requested twice at the upstream at startup (see Cold start) |
| `warmupAuthEnv` | variable holding an `Authorization` value for those requests (e.g. `Bearer <jwt>`) |
| `warmupWait` | how long to wait for the upstream to answer at all (default `2m`) |

## Cold start

At startup, beside serving (never blocking it), each site measures what a first request pays:

- **Database** (`databaseUrlEnv`): the connection time, a `SELECT 1`, then `SELECT 1 FROM <table>
  LIMIT 1` on **every table of the public schema**, read from `pg_catalog`, so it always follows the
  current drizzle migrations without a list to keep up to date. It also reports how many migrations
  `drizzle.__drizzle_migrations` has applied, and warns when `cache.json` names a table the schema does
  not have (its entries would never be invalidated).
- **Upstream** (`warmup`): each path twice, straight at the upstream (nothing is cached). The first time
  pays module compilation, JIT tiering and lazy pools; the second does not. This is the controlled
  warm-up after readiness: users' first product or Empresa search no longer pays it. Without
  `warmupAuthEnv` the ERP answers 401, which still warms routing and middleware but not the queries.

The numbers are logged (`cold start: …`) and sent once as a `cold-start` sample (category
`performance`) to the TCC. The TCC server itself turns on Node's module compile cache
(`NODE_COMPILE_CACHE`) when run under Node; the `tcc-v2.exe` build has its bytecode compiled in.

Only `200` responses without `Set-Cookie` / `Cache-Control: no-store` are kept; a request
with `Cache-Control: no-cache` skips the cache.

## Control (`127.0.0.1:7071`)

- `GET /health` — 200 when every feed is up, 503 when degraded (requests still flow, uncached)
- `GET /stats` — counters per site
- `POST /flush?site=erp` — forget a site (all sites without `site`)

## Notes

- The TCC host tables are written on every telemetry batch, so `/telemetry/host/*` entries live
  seconds at most; the gain there is concurrent dashboards (singleflight) rather than hit rate.
- Triggers are statement-level: a bulk import is one notification per statement, not per row.

## Test

```sh
go vet ./... && go test ./...
```
