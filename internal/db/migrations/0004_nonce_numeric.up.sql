-- 0004_nonce_numeric: fix nonce overflow on real mainnet data.
--
-- BlockHeader.nonce is a proto uint64, but 0002_block_header_decomposition mapped it to
-- a signed BIGINT (max ~9.2e18). Real mainnet nonces routinely exceed that (observed live
-- against node-pool.tari.jagtech.io: height 328606's nonce is 0xed0e4fa15961bcc2 ~=
-- 1.7e19), which crashed the indexer on first contact with live data rather than on any
-- synthetic/edge-case input. This is the same overflow class already fixed for the
-- `nonce` field in go-tari-grpc-lib's blockWinnersCache tool (NUMERIC(20,0) + pgx v5's
-- native uint64<->NUMERIC codec, zero Go-side code changes needed) - applying the
-- identical fix here.
ALTER TABLE blocks ALTER COLUMN nonce TYPE NUMERIC(20,0);
ALTER TABLE blocks ALTER COLUMN nonce SET DEFAULT 0;
