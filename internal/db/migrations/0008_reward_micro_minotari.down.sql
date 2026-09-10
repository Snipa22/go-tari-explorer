-- Revert 0008_reward_micro_minotari.
ALTER TABLE blocks DROP COLUMN IF EXISTS reward_micro_minotari;
