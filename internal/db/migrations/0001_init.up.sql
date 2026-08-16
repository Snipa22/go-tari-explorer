-- 0001_init: minimal-viable schema for v1. Extend incrementally; don't over-build now.

CREATE TABLE IF NOT EXISTS blocks (
    height        BIGINT PRIMARY KEY,
    hash          TEXT NOT NULL,
    prev_hash     TEXT NOT NULL,
    "timestamp"   BIGINT NOT NULL, -- unix seconds, straight from BlockHeader.Timestamp
    pow_algo      TEXT NOT NULL,   -- "RXM" | "RXT" | "C29" | "SHA3X" (see internal/poolattr)
    difficulty    BIGINT NOT NULL DEFAULT 0,
    kernel_count  INTEGER NOT NULL DEFAULT 0,
    output_count  INTEGER NOT NULL DEFAULT 0,
    pool_tag      TEXT -- nullable: NULL when unattributed, see internal/poolattr.BlockAttribution
);

CREATE INDEX IF NOT EXISTS idx_blocks_pow_algo ON blocks (pow_algo);
CREATE INDEX IF NOT EXISTS idx_blocks_pool_tag ON blocks (pool_tag);

-- Minimal per-block kernel/transaction summary. Deliberately not a full per-kernel table
-- yet - one summary row per block height, extended later once real transaction detail
-- pages are needed.
CREATE TABLE IF NOT EXISTS block_kernels (
    block_height  BIGINT PRIMARY KEY REFERENCES blocks (height) ON DELETE CASCADE,
    kernel_count  INTEGER NOT NULL DEFAULT 0,
    total_fee     BIGINT NOT NULL DEFAULT 0
);
