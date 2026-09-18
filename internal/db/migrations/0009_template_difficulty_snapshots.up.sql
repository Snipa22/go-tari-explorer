-- 0009_template_difficulty_snapshots: periodic per-algo "next-block target difficulty"
-- snapshots, populated by cmd/template-difficulty-poller (internal/templatepoller.Poller)
-- polling the LIVE base-node daemon's block-template RPC (GetNewBlockTemplate, via
-- internal/nodeclient - a real GRPC call, not a read of this repo's already-indexed
-- `blocks` table) on a short interval (default 1s, see
-- config.TemplateDifficultyPollInterval).
--
-- This is the FORWARD-LOOKING counterpart to difficulty_snapshots (0007):
-- difficulty_snapshots captures the difficulty a block that was ALREADY MINED actually
-- had (read from the indexed `blocks` table); this table captures the target
-- difficulty for the NEXT block an algo hasn't mined yet (read straight off the live
-- daemon's NewBlockTemplate.header.height / MinerData.target_difficulty). Height here
-- is whatever height the daemon's block template says it's building for, not an
-- indexed-tip+1 assumption.
--
-- Table shape: ONE row per (algo, height) pair that has actually been observed, not one
-- row per poll tick - the poller ticks every ~1s but only inserts a new row when an
-- algo's template height actually advances (detected implicitly via the UNIQUE (algo,
-- height) constraint below, not by the poller tracking state itself - see
-- templatepoller.Poller.Tick). The UNIQUE (algo, height) constraint is what lets the
-- poller's upsert-with-ON-CONFLICT-DO-NOTHING be idempotent/race-safe across ticks
-- without a separate existence-check query, mirroring difficulty_snapshots' exact
-- dedup pattern.
CREATE TABLE IF NOT EXISTS template_difficulty_snapshots (
    id                 BIGSERIAL PRIMARY KEY,
    algo                TEXT NOT NULL,             -- 'RXM' | 'RXT' | 'C29' | 'SHA3X'
    height              BIGINT NOT NULL,            -- height of the NEXT block this template targets (from NewBlockTemplate.header.height)
    target_difficulty   BIGINT NOT NULL,            -- MinerData.target_difficulty from GetNewBlockTemplate at capture time
    reward              BIGINT NOT NULL,            -- MinerData.reward, captured for free alongside difficulty
    recorded_at         TIMESTAMPTZ NOT NULL,
    UNIQUE (algo, height)
);

-- Front page / future "current template difficulty" reads want "latest row per algo"
-- (DISTINCT ON (algo) ... ORDER BY algo, height DESC) - this index makes that a cheap
-- index-only scan rather than a table scan, mirroring
-- idx_difficulty_snapshots_algo_height.
CREATE INDEX IF NOT EXISTS idx_template_difficulty_snapshots_algo_height ON template_difficulty_snapshots (algo, height DESC);

-- Supports future "template difficulty over time" queries/charts bounded by
-- recorded_at, mirroring idx_difficulty_snapshots_recorded_at.
CREATE INDEX IF NOT EXISTS idx_template_difficulty_snapshots_recorded_at ON template_difficulty_snapshots (recorded_at);
