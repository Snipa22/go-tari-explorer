# Changelog

## 1.0.0 (2026-08-30)


### Features

* **analysis:** add historical analysis views (algo distribution, pool share, block time, difficulty) ([4bd495c](https://github.com/Snipa22/go-tari-explorer/commit/4bd495c9730fafac28c3d0ef867b2ee1bcffd394))
* **analysis:** add per-bucket data tables to historical-analysis views ([61a157b](https://github.com/Snipa22/go-tari-explorer/commit/61a157b67a25b4977fc095e798f95faee4144442))
* **analysis:** add per-bucket mean/median/stddev/max to block-time table ([4269239](https://github.com/Snipa22/go-tari-explorer/commit/4269239c6243b7da16888005c119c6df0d9013e8))
* **analysis:** default historical-analysis views to last 10,000 blocks ([4b541be](https://github.com/Snipa22/go-tari-explorer/commit/4b541beeb50b1b88bd21879e75b14fb50d8f48b3))
* **analysis:** general pool-tag-family merge + per-pool algo breakdown ([5ae821c](https://github.com/Snipa22/go-tari-explorer/commit/5ae821c2b15bbe8358b9bc9815f9b6fed59f5306))
* **db:** add height-bucketed pow-algo aggregation query and cmd/algobuckets ([62b6ee1](https://github.com/Snipa22/go-tari-explorer/commit/62b6ee11c73d47b164ec540b6bd52896d1082aa4))
* **db:** decompose blocks table into full BlockHeader+ProofOfWork schema ([b182cd5](https://github.com/Snipa22/go-tari-explorer/commit/b182cd5fe98e9caf0dbb49e9ef5d74be7fee5599))
* **difficulty:** add live per-algo current-difficulty stat cards ([0a5ff07](https://github.com/Snipa22/go-tari-explorer/commit/0a5ff07780945c869eee9bf6b90b62964acb6b8c))
* **mempool:** nodeclient GRPC wrapper + mempool_snapshots schema + poller ([4a482be](https://github.com/Snipa22/go-tari-explorer/commit/4a482be3d446f4b7432759f4fa3b4cb470777c4c))
* **poolattr:** recognize GCPOOL-SOLO as legacy SupportXTM own-pool tag ([1766685](https://github.com/Snipa22/go-tari-explorer/commit/1766685db99adcfa21aca0640a31ff2e8499ce40))
* **poolattr:** recognize supportxtm- as a second own-pool prefix ([1f3e1a0](https://github.com/Snipa22/go-tari-explorer/commit/1f3e1a03a52429e4b23a1f20c9aa518080c04748))
* **poolstats:** add nodejs-pool stats client and pool-stats page ([8cc9061](https://github.com/Snipa22/go-tari-explorer/commit/8cc9061eedbdae7adfd4b1d9c1737e3facb0e4c3))
* **reattribute:** add one-shot pool_tag recompute CLI tool ([8a8aea1](https://github.com/Snipa22/go-tari-explorer/commit/8a8aea19b95744500fcd7894f95856c934bf090c))
* scaffold go-tari-explorer v1 (nodeclient, poolattr, db, indexer, server) ([95a764f](https://github.com/Snipa22/go-tari-explorer/commit/95a764feda00f70d850a557831ec76073d26bbfb))
* **server:** dense table layout for front-page At-a-Glance panel ([be2c3b0](https://github.com/Snipa22/go-tari-explorer/commit/be2c3b09571c011a2a3601d3c0f45f57687d6eab))
* **server:** front-page recent-blocks/mempool stats panels + /mempool routes ([5cc1475](https://github.com/Snipa22/go-tari-explorer/commit/5cc147531fe02482c7cbb5b14923dbfe2cff1283))
* **server:** humanize large numbers with comma thousands-grouping ([0e8b2d1](https://github.com/Snipa22/go-tari-explorer/commit/0e8b2d1b988b6a05c77430be2fba480d44da7ba6))
* **server:** move current difficulty into algo glance table ([241eb72](https://github.com/Snipa22/go-tari-explorer/commit/241eb7242396a8f844560af2c75523c7c3ed33e4))
* **server:** pre-launch hardening for public exposure at explore.tari.jagtech.io ([f7298c6](https://github.com/Snipa22/go-tari-explorer/commit/f7298c60a3b11556ff649ac548179fdb3f585fc0))
* **server:** split front-page avg difficulty per-algo in Algo column ([66c23ac](https://github.com/Snipa22/go-tari-explorer/commit/66c23ac767c50c2d15ea4234ebdbff0fac40200b))
* **server:** split front-page stats into Pool/Algo/Mempool columns ([cd6e698](https://github.com/Snipa22/go-tari-explorer/commit/cd6e698ba64071a0fb157d1b5c32a3ffbfa0865e))
* **txcheck:** add per-kernel/output persistence, search, and live tx-state lookup ([448c4c6](https://github.com/Snipa22/go-tari-explorer/commit/448c4c6911e0fb4201b0b680dfe4f3b6b24980b8))


### Bug Fixes

* **analysis:** humanize block-time table bucket/sample columns ([46f3ee4](https://github.com/Snipa22/go-tari-explorer/commit/46f3ee4dd1430a13f32a36ad956f9c3d9bc80651))
* **analysis:** split difficulty chart into per-algo series ([0b3e8a2](https://github.com/Snipa22/go-tari-explorer/commit/0b3e8a2a49a43aa49e9886a8d121bdd21ce45000))
* **db:** outputs.maturity overflow on real mainnet data ([4b0ef95](https://github.com/Snipa22/go-tari-explorer/commit/4b0ef95b5c07bfddce07e6a6810648634ab49392))
* **poolattr:** populate PoolTag with printable label on unknown coinbase-extra fallback ([0d0fe76](https://github.com/Snipa22/go-tari-explorer/commit/0d0fe7603b185782a5d9359c2b34daa75427a60f))
* **poolattr:** truncate own-pool tags to canonical 12-byte identifier ([5ae821c](https://github.com/Snipa22/go-tari-explorer/commit/5ae821c2b15bbe8358b9bc9815f9b6fed59f5306))
