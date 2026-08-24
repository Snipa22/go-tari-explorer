-- 0006_mempool_snapshots: periodic aggregate mempool snapshots, populated by
-- cmd/mempool-poller (internal/mempoolpoller.Poller) polling the base node's live
-- GetMempoolStats RPC (see internal/nodeclient.Client.GetMempoolStats) on an interval.
--
-- Table shape: ONE row per poll tick, summarizing the entire mempool at that instant -
-- not one row per pending transaction. There is deliberately no per-transaction detail
-- table here: mempool contents are inherently transient (a transaction can be mined,
-- evicted, or replaced between any two polls), so persisting per-transaction rows would
-- immediately go stale and would need its own reconciliation logic for a "v1, minimal-
-- viable, don't front-load speculative schema" feature (see AGENTS.md) whose only
-- current consumer is a small number of aggregate-over-time charts/stat cards. Live
-- per-transaction mempool detail, if ever needed, should be fetched fresh via
-- internal/nodeclient.Client.GetMempoolTransactions rather than read from this table.
--
-- Field types mirror the real wire types from go-tari-grpc-lib/v3's
-- MempoolStatsResponse (base_node.proto): unconfirmed_txs/reorg_txs are proto uint64
-- but in practice bounded by "how many transactions can plausibly sit in one mempool"
-- (nowhere near even INTEGER's ~2.1 billion ceiling), so INTEGER is sufficient and
-- matches this schema's existing kernel_count/output_count convention
-- (0001_init.up.sql). unconfirmed_weight is also a proto uint64 but, unlike
-- OutputFeatures.maturity/BlockHeader.nonce (see 0004_nonce_numeric,
-- 0005_maturity_numeric - both NUMERIC(20,0) because those fields are attacker/miner-
-- controlled and were observed to hit real uint64-max values on mainnet), mempool
-- weight is a computed, practically-bounded sum over the current mempool's actual
-- transactions - it has no plausible path to approaching BIGINT's ~9.2e18 ceiling, so
-- plain BIGINT (this schema's existing convention for "wire uint64, no observed/
-- plausible overflow risk", e.g. blocks.difficulty, kernels.fee) is used instead of
-- NUMERIC.
--
-- id is a surrogate BIGSERIAL rather than a natural key: unlike blocks (keyed on
-- height) or kernels/outputs (keyed on (block_height, index)), a poll tick has no
-- natural identity of its own beyond "when it happened", and snapshot_time is not
-- declared UNIQUE (multiple pollers, clock skew, or a sub-second poll interval could
-- all plausibly produce duplicate/out-of-order timestamps - this table has no business
-- asserting otherwise).
CREATE TABLE IF NOT EXISTS mempool_snapshots (
    id                  BIGSERIAL PRIMARY KEY,
    snapshot_time       TIMESTAMPTZ NOT NULL, -- when this poll tick was taken
    unconfirmed_txs     INTEGER NOT NULL DEFAULT 0, -- MempoolStatsResponse.unconfirmed_txs
    reorg_txs           INTEGER NOT NULL DEFAULT 0, -- MempoolStatsResponse.reorg_txs
    unconfirmed_weight  BIGINT NOT NULL DEFAULT 0 -- MempoolStatsResponse.unconfirmed_weight
);

CREATE INDEX IF NOT EXISTS idx_mempool_snapshots_snapshot_time ON mempool_snapshots (snapshot_time);
