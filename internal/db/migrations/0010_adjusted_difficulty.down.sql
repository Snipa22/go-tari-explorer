-- Revert 0010_adjusted_difficulty.
ALTER TABLE blocks DROP COLUMN IF EXISTS adjusted_difficulty;
ALTER TABLE difficulty_snapshots DROP COLUMN IF EXISTS adjusted_difficulty;
