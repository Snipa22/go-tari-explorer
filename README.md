# go-tari-explorer

v1 of a Tari block explorer: indexes blocks from one or more Tari base-node GRPC hosts into Postgres (with pool attribution on each block's coinbase), and serves a minimal server-rendered HTML UI over that data. Part of the [`go-tari-*`](https://github.com/Snipa22?tab=repositories&q=go-tari) ecosystem maintained by [Snipa22](https://github.com/Snipa22), depending on [go-tari-grpc-lib](https://github.com/Snipa22/go-tari-grpc-lib) for the GRPC wrapper/generated protobuf types.

This is a **foundational v1** — a working skeleton with real (not stubbed) core pieces: a real multi-host GRPC client with failover, a real pool-attribution parser (with tests against known real-world data), a real Postgres schema/indexer, and a real (if minimal) server-rendered UI. It is not the full eventual feature set — see "What's deliberately deferred" below.

## Architecture

```
cmd/indexer   -> internal/indexer -> internal/nodeclient (multi-host GRPC + failover)
                                   -> internal/poolattr   (coinbase -> pool attribution)
                                   -> internal/db         (Postgres upsert)

cmd/server    -> internal/server  -> internal/db         (Postgres read, paginated)
```

- **`internal/nodeclient`** — wraps `go-tari-grpc-lib/v3`'s generated `tari_generated.BaseNodeClient` with support for a *list* of base-node host:port targets and basic failover (try the next host on error, remember which host last worked). Deliberately bypasses go-tari-grpc-lib's `nodeGRPC` package, which holds its connection in an unexported package-level global unsafe for polling multiple hosts — see the doc comment in `nodeclient.go` for the full rationale.
- **`internal/poolattr`** — rebuilds (not ports verbatim) the pool-attribution logic from `go-tari-grpc-lib/cmd/blockWinners/main.go` into a structured `BlockAttribution` struct (`BlockHeight`, `PowAlgo`, `PoolTag`, `RawExtra`, `IsOwnPool`) instead of a `fmt.Println` CLI report. Known pool prefixes (own tags `WUF*`, external pools like `pool.kryptex.com`, `LuckyPool`, `hash2coin`, etc.) live in a lookup table with a documented fallback bucket for anything unrecognized.
- **`internal/db`** — Postgres access plus a small hand-rolled embedded-SQL migration runner (see "Migrations" below for why this isn't golang-migrate yet).
- **`internal/indexer`** — walks blocks in batches, runs attribution, upserts into Postgres. Supports a one-shot `-mode=backfill` and a polling `-mode=follow` ("keep following the tip").
- **`internal/server`** — `net/http` + `html/template`, HTMX (loaded from a CDN `<script>` tag, no separate JS build step) driving the blocks-list "load more" pagination, plus a pool-stats page (see below).
- **`internal/poolstats`** — HTTP client for a mining pool's stats API, behind a small `PoolStatsProvider` interface so `internal/server` never depends on a specific backend's JSON shape. The current implementation (`HTTPClient`) talks to a nodejs-pool/node-cryptonote-pool-derived backend (live at `pool.rxt.tari.jagtech.io`) via `GET /api/pool/stats`. This backend is expected to be rebuilt/replaced over time (ties to a separate SupportXMR Go-rewrite effort) — swap in a new `PoolStatsProvider` implementation rather than growing `HTTPClient` to match a new shape.

## Schema (v1, minimal-viable)

Two tables (see `internal/db/migrations/0001_init.up.sql`):

- `blocks` — `height` (PK), `hash`, `prev_hash`, `timestamp`, `pow_algo`, `difficulty`, `kernel_count`, `output_count`, `pool_tag` (nullable).
- `block_kernels` — a per-block kernel-count/fee summary row, keyed on `block_height`. Not yet a full per-kernel table — extend incrementally as real transaction-detail pages are needed.

## Migrations

Migrations are plain `.up.sql`/`.down.sql` files under `internal/db/migrations/`, embedded into the binary via `//go:embed` and applied by a small hand-rolled runner in `internal/db/db.go` (`DB.Migrate`) that tracks applied versions in a `schema_migrations` table. This intentionally avoids pulling in `golang-migrate` for what is currently a single `CREATE TABLE` migration — the files are already named in golang-migrate's `<version>_<name>.up.sql`/`.down.sql` convention, so swapping to it later (once down-migrations/dirty-state handling actually earns its keep) is a drop-in change, not a rewrite.

## Configuration

All config is env var / CLI flag driven with local-dev defaults — no hardcoded secrets, no required config file.

| Setting | Flag | Env var | Default |
|---|---|---|---|
| Postgres DSN | `-postgres-dsn` | `TARI_EXPLORER_POSTGRES_DSN` | `postgres://tari_explorer:***@localhost:5432/tari_explorer?sslmode=disable` |
| Base-node GRPC hosts (comma-separated) | `-base-node-grpc-hosts` | `TARI_EXPLORER_NODE_HOSTS` | `node-pool.tari.jagtech.io:18102` |
| HTTP listen address (server only) | `-http-addr` | `TARI_EXPLORER_HTTP_ADDR` | `:8080` |
| Pool stats API base URL (server only) | `-pool-stats-base-url` | `TARI_EXPLORER_POOL_STATS_BASE_URL` | `https://pool.rxt.tari.jagtech.io` |

## Running locally

```bash
# 1. Postgres (any local instance works; the indexer/server both run migrations on startup)
export TARI_EXPLORER_POSTGRES_DSN="postgres://tari_explorer:tari_explorer@localhost:5432/tari_explorer?sslmode=disable"

# 2. One-shot backfill of the last 1000 blocks from the tip
go run ./cmd/indexer -mode=backfill -from=<tip-1000>

# 3. Or keep following the tip
go run ./cmd/indexer -mode=follow -poll-interval=30s

# 4. Serve the UI
go run ./cmd/server -http-addr=:8080
```

Point `-base-node-grpc-hosts` (or `TARI_EXPLORER_NODE_HOSTS`) at a comma-separated list for redundancy, e.g. `node1.example.com:18102,node2.example.com:18102`.

## Pool Stats page

`GET /pool-stats` renders pool-wide statistics (hash rate, connected miners, round hashes, total hashes/blocks found, last block/payment timestamps) fetched live from the configured pool stats backend (`-pool-stats-base-url` / `TARI_EXPLORER_POOL_STATS_BASE_URL`, default `https://pool.rxt.tari.jagtech.io`). A nav link to it is on every page.

This intentionally only surfaces pool-wide numbers — no per-miner/per-worker/per-address data is fetched, modeled, or displayed anywhere. Tari is MimbleWimble (no public address model), and no per-miner endpoint is confirmed to exist on the current backend; this is a deliberate scope boundary, not a gap to fill later without re-confirming a real endpoint first.

`internal/poolstats.PoolStatsProvider` is the seam behind this page — the current nodejs-pool-derived `HTTPClient` implementation is expected to be swapped out over time as the backend it talks to gets rebuilt/replaced.

## What's deliberately deferred (not in v1)

- Per-kernel/per-transaction detail beyond the `block_kernels` summary row.
- Active health-checking of base-node hosts (failover is reactive — try the next host only on a failed call, not proactive).
- `golang-migrate`-style down-migrations/dirty-state recovery (see "Migrations" above).
- Pagination beyond simple height-cursor "load more" (no jump-to-page, no search).
- Real difficulty lookups are best-effort per block via `GetNetworkDifficulty`; a failed lookup doesn't fail the whole block, it just leaves `difficulty = 0`.
- Pool-stats page only surfaces `/api/pool/stats`; `/api/network/stats` and `/api/pool/blocks` (and the `finder` field on the latter, which is currently a hardcoded placeholder server-side) are not wired up yet.
- No per-miner/per-worker/per-address pool data anywhere (privacy — Tari is MimbleWimble, no public address model; no such endpoint is confirmed to exist regardless).

## License

MIT — see `LICENSE`.
