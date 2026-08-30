-- 0007_difficulty_snapshots: periodic per-algo "current live difficulty" snapshots,
-- populated by cmd/difficulty-poller (internal/difficultypoller.Poller) polling this
-- same database's already-indexed `blocks` table (not a live base-node GRPC call - see
-- internal/db.CurrentDifficultyPerAlgo's doc comment for why the difficulty stamped on
-- an algo's single most-recently-indexed block already IS that algo's live current
-- target, since RXM/RXT/C29/SHA3X each run their own independent difficulty-adjustment
-- window) on a short interval (default 1s, see config.DifficultyPollInterval).
--
-- Table shape: ONE row per (algo, height) pair that has actually been observed, not one
-- row per poll tick - the poller ticks every ~1s but only inserts a new row when an
-- algo's latest indexed height actually advances (see difficultypoller.Poller.Tick),
-- so this table only grows on real new blocks, at the natural per-algo block rate, not
-- at the poll rate. The UNIQUE (algo, height) constraint is what lets the poller's
-- upsert-with-ON-CONFLICT-DO-NOTHING be idempotent/race-safe across ticks without a
-- separate existence-check query.
--
-- Unlike mempool_snapshots (0006, deliberately no natural key - every tick is a
-- distinct point-in-time observation with nothing to de-duplicate against), this table
-- has a real, meaningful history: "difficulty over time, per algo" - so its rows are
-- worth keeping and querying by time range later (Alex: "it'd be interesting over
-- time"), not just read as an always-overwritten single "current" row per algo.
CREATE TABLE IF NOT EXISTS difficulty_snapshots (
    id          BIGSERIAL PRIMARY KEY,
    algo        TEXT NOT NULL,       -- 'RXM' | 'RXT' | 'C29' | 'SHA3X', matches blocks.pow_algo
    height      BIGINT NOT NULL,     -- height of the algo-specific block this difficulty came from
    difficulty  BIGINT NOT NULL,     -- blocks.difficulty at that height (that algo's live target)
    recorded_at TIMESTAMPTZ NOT NULL, -- when the poller observed/recorded this (height, algo) pair
    UNIQUE (algo, height)
);

-- Front page reads "latest row per algo" (DISTINCT ON (algo) ... ORDER BY algo, height
-- DESC) - this index makes that a cheap index-only scan rather than a table scan.
CREATE INDEX IF NOT EXISTS idx_difficulty_snapshots_algo_height ON difficulty_snapshots (algo, height DESC);

-- Supports future "difficulty over time" queries/charts bounded by recorded_at, mirroring
-- mempool_snapshots' idx_mempool_snapshots_snapshot_time.
CREATE INDEX IF NOT EXISTS idx_difficulty_snapshots_recorded_at ON difficulty_snapshots (recorded_at);
