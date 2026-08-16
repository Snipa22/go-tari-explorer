---
description: go-tari-* ecosystem repo — Tari network Go tooling
---

# AGENTS.md

Instructions for AI coding agents (OpenCode, Claude Code, or any `agents.md`-compatible tool) working in this repository. Read this before making changes.

## Project

- **What this repo is:** v1 of a Tari block explorer — indexes blocks from one or more base-node GRPC hosts into Postgres (with pool attribution on each block's coinbase), and serves a minimal server-rendered HTML UI (blocks list + block detail) over that data. Foundational/v1 scope: a working skeleton with real (not stub) core pieces, not the full eventual feature set.
- **Module path:** `github.com/Snipa22/go-tari-explorer`
- **Depends on:** `go-tari-grpc-lib` (GRPC wrapper/generated protobuf types)
- **Versioning note:** single-module repo — uses release-please's `release-type: simple`, not monorepo/per-path mode (contrast with `go-tari-tools`, which aggregates multiple binaries and does use per-path versioning).

## Layout

- `internal/nodeclient` — multi-host base-node GRPC client with basic failover. **Deliberately dials its own `*grpc.ClientConn` per host and calls `tari_generated.NewBaseNodeClient` directly, instead of using go-tari-grpc-lib's `nodeGRPC` package.** `nodeGRPC` holds its connection in an unexported package-level global (`InitNodeGRPC` singleton) which is unsafe for polling multiple hosts concurrently. This is a deliberate design choice, not an oversight — see the doc comment at the top of `nodeclient.go`. If a future go-tari-grpc-lib release exposes a proper per-connection constructor, adopt it and delete this workaround.
- `internal/poolattr` — structured pool-attribution logic, rebuilt (not ported verbatim) from `go-tari-grpc-lib/cmd/blockWinners/main.go`. Returns a `BlockAttribution` struct instead of printing. Known pool prefixes live in a table (`prefixTable`) — add new pools there, not as another `if`/`HasPrefix` chain.
- `internal/db` — Postgres access + a small hand-rolled embedded-SQL migration runner (see doc comment in `db.go` for why this isn't golang-migrate yet).
- `internal/indexer` — walks blocks via `nodeclient`, attributes via `poolattr`, upserts via `db`. Supports one-shot `Backfill` and polling `Follow` modes.
- `internal/server` — minimal `net/http` + `html/template` server; HTMX (CDN script tag, no build step) drives the blocks-list "load more" pagination.
- `internal/config` — env var / CLI flag resolution with local-dev defaults. No hardcoded secrets.
- `cmd/indexer` — CLI entrypoint for the indexer (`-mode=backfill|follow`).
- `cmd/server` — CLI entrypoint for the HTTP UI.

## Commands

- **Build:** `go build ./...`
- **Test:** `go test ./... -count=1`
- **Vet:** `go vet ./...`
- **Format:** `gofmt -l .` (should return nothing; `gofmt -w .` to fix)
- **Tidy:** `go mod tidy`

Run build + vet + gofmt + test before considering any change complete. CI will re-check all four; catch failures locally first.

## Conventions

- **Conventional Commits** required — commit type (`feat`/`fix`/`chore`/etc.) drives automated SemVer via release-please. Don't guess the type; pick the one that matches the actual change.
- **Rebase, never merge.** No merge commits in PR branches. Rebase onto `main` before pushing updates.
- **No direct commits/pushes to `main`.** Always via PR.
- Follow existing package structure and naming — don't introduce a new pattern without checking how sibling `go-tari-*` repos do it first.
- Pin dependency versions explicitly in `go.mod` — this ecosystem has a known history of version skew across repos on `go-tari-grpc-lib`; don't make it worse.
- Schema in `internal/db/migrations` is intentionally minimal-viable (v1) — extend it incrementally as real features need new columns/tables, don't front-load speculative schema.

## Don't

- Don't push directly to `main` or force-push shared branches.
- Don't add merge commits — rebase instead.
- Don't touch generated/vendored code (anything under a `_generated`, `tari_generated`, `tari_protos`, or similar directory in a *dependency* — this repo has none of its own) by hand.
- Don't silently change the licensing header or LICENSE file — that's a human decision, flag it instead.
- Don't skip tests because "there weren't any before" — add coverage for what you touch, especially `internal/poolattr` (the parsing logic is exactly the kind of ad-hoc string matching that regresses silently).
- Don't reach for `go-tari-grpc-lib`'s `nodeGRPC` singleton wrapper for anything that needs to talk to more than one host — see the `internal/nodeclient` note above.

## Disclosure

If you (the agent) are making a substantial autonomous contribution, make sure the human operator adds a disclosure note to the PR per `CONTRIBUTING.md`. Don't assume this happens automatically — mention it if it's about to be skipped.
