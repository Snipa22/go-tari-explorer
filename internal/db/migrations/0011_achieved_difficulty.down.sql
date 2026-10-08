-- Revert 0011_achieved_difficulty.
ALTER TABLE blocks DROP COLUMN IF EXISTS achieved_difficulty;
