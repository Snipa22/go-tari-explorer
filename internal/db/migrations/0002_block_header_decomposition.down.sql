-- 0002_block_header_decomposition: reverse the BlockHeader/ProofOfWork decomposition
-- back to 0001_init's flattened shape.

ALTER TABLE blocks
    DROP COLUMN IF EXISTS version,
    DROP COLUMN IF EXISTS output_mr,
    DROP COLUMN IF EXISTS block_output_mr,
    DROP COLUMN IF EXISTS kernel_mr,
    DROP COLUMN IF EXISTS input_mr,
    DROP COLUMN IF EXISTS total_kernel_offset,
    DROP COLUMN IF EXISTS nonce,
    DROP COLUMN IF EXISTS pow_algo_raw,
    DROP COLUMN IF EXISTS pow_data,
    DROP COLUMN IF EXISTS kernel_mmr_size,
    DROP COLUMN IF EXISTS output_mmr_size,
    DROP COLUMN IF EXISTS total_script_offset,
    DROP COLUMN IF EXISTS validator_node_mr,
    DROP COLUMN IF EXISTS validator_node_size;
