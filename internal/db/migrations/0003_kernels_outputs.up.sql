-- 0003_kernels_outputs: adds real per-kernel and per-output rows for each block, so
-- block detail pages and transaction search can show/find actual transaction data
-- instead of just the summary kernel_count/output_count integers `blocks` has carried
-- since 0001_init.
--
-- Why now: the explorer's block detail page only rendered header fields plus two
-- counts - there was no way to show what a block's kernels/outputs actually *were*, and
-- no way to answer "does this excess signature / commitment / payment reference appear
-- anywhere in the chain" without a live GRPC search. Both are now required (see
-- internal/server's new /search route and internal/txsearch), and both need real rows
-- to query against for the fast, already-indexed path.
--
-- NOTE on the pre-existing `block_kernels` table (from 0001_init): that table is a
-- one-row-per-block *summary* (kernel_count, total_fee) and is left untouched here. It
-- is now redundant with `kernels` below (COUNT(*)/SUM(fee) over `kernels` per
-- block_height gives the same numbers), but nothing currently reads or writes it beyond
-- its own migration, and removing/merging it is a schema decision for the repo owner,
-- not something this migration should decide unilaterally.
--
-- Table shape: one row per kernel/output per block, keyed on (block_height, index
-- within block) rather than a surrogate id, since that composite is already the
-- natural identity of "the Nth kernel/output in block H" and lets DELETE FROM ... WHERE
-- block_height = $1 do a full, idempotent re-index of a block's children in one
-- statement (see internal/indexer.go's indexBlock).
--
-- Field types mirror the real wire types from go-tari-grpc-lib/v3's transaction.proto
-- (TransactionKernel, TransactionOutput, OutputFeatures): proto `bytes` -> BYTEA,
-- proto uint32/uint64 -> BIGINT (Postgres has no unsigned integer type, same rationale
-- as 0002's header decomposition).
--
-- Indexing: `excess_sig_signature` and `commitment` are NOT declared UNIQUE.
-- Commitments are Pedersen commitments (blinding_factor*G + value*H) - collision-free
-- in practice because the blinding factor is drawn from a large random field, but
-- nothing in the protocol *forbids* the same commitment value appearing more than once
-- across the chain's history (e.g. degenerate/adversarial or replayed-value cases), so a
-- UNIQUE constraint would be asserting a protocol invariant this table has no business
-- asserting. Same reasoning for excess_sig_signature. Plain non-unique btree indexes are
-- sufficient for the O(1)-ish equality lookups /search needs.

CREATE TABLE IF NOT EXISTS kernels (
    block_height        BIGINT NOT NULL REFERENCES blocks (height) ON DELETE CASCADE,
    kernel_index         INTEGER NOT NULL, -- position of this kernel within the block's body.kernels
    features             BIGINT NOT NULL DEFAULT 0, -- TransactionKernel.features bitmask
    fee                  BIGINT NOT NULL DEFAULT 0, -- MicroMinotari, TransactionKernel.fee
    lock_height          BIGINT NOT NULL DEFAULT 0,
    excess               BYTEA NOT NULL DEFAULT ''::bytea,
    excess_sig_nonce     BYTEA NOT NULL DEFAULT ''::bytea, -- Signature.public_nonce
    excess_sig_signature BYTEA NOT NULL DEFAULT ''::bytea, -- Signature.signature - the canonical per-transaction identifier
    hash                 BYTEA NOT NULL DEFAULT ''::bytea, -- kernel hash as it appears in the MMR
    PRIMARY KEY (block_height, kernel_index)
);

CREATE INDEX IF NOT EXISTS idx_kernels_excess_sig_signature ON kernels (excess_sig_signature);
CREATE INDEX IF NOT EXISTS idx_kernels_excess_sig_full ON kernels (excess_sig_nonce, excess_sig_signature);

CREATE TABLE IF NOT EXISTS outputs (
    block_height     BIGINT NOT NULL REFERENCES blocks (height) ON DELETE CASCADE,
    output_index      INTEGER NOT NULL, -- position of this output within the block's body.outputs
    features_version  INTEGER NOT NULL DEFAULT 0, -- OutputFeatures.version
    output_type       INTEGER NOT NULL DEFAULT 0, -- OutputFeatures.output_type (0=STANDARD,1=COINBASE,2=BURN,3=VALIDATOR_NODE_REGISTRATION,4=CODE_TEMPLATE_REGISTRATION)
    maturity          BIGINT NOT NULL DEFAULT 0, -- OutputFeatures.maturity
    coinbase_extra    BYTEA NOT NULL DEFAULT ''::bytea, -- OutputFeatures.coinbase_extra, display-only here (see internal/poolattr for the pool-tagging use of this same field)
    commitment        BYTEA NOT NULL DEFAULT ''::bytea, -- TransactionOutput.commitment - searchable via SearchUtxos
    PRIMARY KEY (block_height, output_index)
);

CREATE INDEX IF NOT EXISTS idx_outputs_commitment ON outputs (commitment);
