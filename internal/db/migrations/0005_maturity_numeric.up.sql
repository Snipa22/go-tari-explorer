-- 0005_maturity_numeric: fix maturity overflow on real mainnet data.
--
-- OutputFeatures.maturity is a proto uint64, but 0003_kernels_outputs mapped it to a
-- signed BIGINT (max ~9.2e18) - same overflow class already fixed for blocks.nonce in
-- 0004_nonce_numeric. Real mainnet data hit this immediately on the historical
-- kernels/outputs backfill: height 240592's output has maturity 0xffffffffffffffff
-- (u64::MAX, 18446744073709551615), which is >9.2e18 and crashed the backfill outright
-- rather than on any synthetic edge case. Applying the identical NUMERIC(20,0) fix used
-- for nonce - pgx v5's native uint64<->NUMERIC codec handles it, zero Go code changes.
ALTER TABLE outputs ALTER COLUMN maturity TYPE NUMERIC(20,0);
ALTER TABLE outputs ALTER COLUMN maturity SET DEFAULT 0;
