-- 0003_analysis_indexes (down): drop the two composite indexes added for the
-- historical-analysis feature's bucketed queries. Safe to drop independently of
-- 0001_init's height PK / idx_blocks_pow_algo / idx_blocks_pool_tag, which this
-- migration never touched.

DROP INDEX IF EXISTS idx_blocks_height_pool_tag;
DROP INDEX IF EXISTS idx_blocks_height_difficulty;
