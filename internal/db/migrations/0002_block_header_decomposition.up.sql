-- 0002_block_header_decomposition: adds the full BlockHeader + ProofOfWork field set
-- (per go-tari-grpc-lib v3.2.0's block.proto) onto the existing `blocks` table. Header
-- and pow are 1:1 per block, so both are flattened into this same row rather than a
-- separate pow table. `height` stays the PRIMARY KEY, so block_kernels' existing FK
-- reference is unaffected by this migration.
--
-- hash/prev_hash/pow_algo (string)/pool_tag/kernel_count/output_count already existed
-- from 0001_init and are left untouched here: hash/prev_hash stay hex-encoded TEXT (as
-- rendered verbatim by internal/server's templates) rather than being converted to the
-- proto's raw BYTEA, to avoid a breaking change to the UI layer; pow_algo (string,
-- "RXM"/"RXT"/"C29"/"SHA3X") stays as internal/poolattr's classification.
--
-- Every other BlockHeader/ProofOfWork field 0001_init didn't carry is added below,
-- using the real proto's wire types (bytes -> BYTEA, uint32/uint64 -> BIGINT, since
-- Postgres has no unsigned integer type). pow_algo_raw is the raw
-- ProofOfWork.pow_algo id straight off the wire (0=RXM, 1=SHA3X, 2=RXT, 3=C29, per the
-- proto comment) and intentionally coexists with the existing string pow_algo column
-- (internal/poolattr.AlgoFromRaw's classification of that same raw id) rather than
-- replacing it.

ALTER TABLE blocks
    ADD COLUMN IF NOT EXISTS version              BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS output_mr             BYTEA  NOT NULL DEFAULT ''::bytea,
    ADD COLUMN IF NOT EXISTS block_output_mr       BYTEA  NOT NULL DEFAULT ''::bytea,
    ADD COLUMN IF NOT EXISTS kernel_mr             BYTEA  NOT NULL DEFAULT ''::bytea,
    ADD COLUMN IF NOT EXISTS input_mr              BYTEA  NOT NULL DEFAULT ''::bytea,
    ADD COLUMN IF NOT EXISTS total_kernel_offset   BYTEA  NOT NULL DEFAULT ''::bytea,
    ADD COLUMN IF NOT EXISTS nonce                 BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS pow_algo_raw          BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS pow_data              BYTEA  NOT NULL DEFAULT ''::bytea,
    ADD COLUMN IF NOT EXISTS kernel_mmr_size       BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS output_mmr_size       BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS total_script_offset   BYTEA  NOT NULL DEFAULT ''::bytea,
    ADD COLUMN IF NOT EXISTS validator_node_mr     BYTEA  NOT NULL DEFAULT ''::bytea,
    ADD COLUMN IF NOT EXISTS validator_node_size   BIGINT NOT NULL DEFAULT 0;
