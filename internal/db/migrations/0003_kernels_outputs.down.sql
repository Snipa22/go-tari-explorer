-- Reverses 0003_kernels_outputs.up.sql. Drops the per-kernel/per-output detail tables
-- added for block-detail rendering and transaction search; does not touch the
-- pre-existing, still-untouched `block_kernels` summary table.

DROP TABLE IF EXISTS outputs;
DROP TABLE IF EXISTS kernels;
