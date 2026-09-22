// This file adds a dedicated /api/* JSON namespace alongside internal/server's
// existing HTML+HTMX views (server.go/analysis.go/mempool.go), per
// DISPATCH_BRIEF_JSON_API_PHASE1.md: a Tari user asked for a JSON API "similar to
// https://textexplore.tari.com/?json" on this (L1) repo, matching the settled design
// already shipped on the sibling go-tari-ootle-explorer (L2/Ootle) repo - a SEPARATE
// `/api/*` route namespace, not a `?json=1` query-param alias bolted onto the existing
// HTML routes (which this file leaves entirely untouched).
//
// This is Phase 1 of 2: core Postgres-backed data (blocks/pool-stats/analysis) plus the
// real live tip-info and a health check. Phase 2 (a separate dispatch) covers
// search/tx-state/mempool - the live-GRPC, rate-limited routes not touched here.
//
// Every /api/* route below mirrors one existing HTML route's Store/DB query and
// pagination/filter semantics (see each handler's own doc comment for exactly which
// one), but returns the raw db.*/poolstats.* row types (JSON-tagged directly on the
// structs themselves - see db.Block/db.Kernel/db.Output/db.*BucketRow's own doc
// comments in internal/db, and poolstats.PoolStats's own doc comment) instead of going
// through server.go's HTML-only view adapters (blockView, poolStatsView, etc.) - those
// exist purely for template display methods like TimeString()/HashRateDisplay() and
// have no bearing on this JSON surface.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/Snipa22/go-tari-explorer/internal/analysis"
	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// MaxAPILimit caps a client-supplied ?limit= on /api/blocks - a JSON API consumer
// could otherwise request an unbounded page size, unlike the HTML/HTMX blocks list
// which only ever requests PageSize at a time.
const MaxAPILimit = 100

// writeJSON marshals v as the response body with the given HTTP status and the
// application/json; charset=utf-8 content type every /api/* route uses.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("server: api: encode json response: %v", err)
	}
}

// writeJSONError writes a `{"error": "<message>"}` JSON body at the given status -
// every /api/* route's error path, replacing the HTML routes' http.Error (plain-text
// body) equivalent, so a JSON API consumer never has to handle a non-JSON error body.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// parseAPILimit parses a ?limit= query param value, defaulting to defaultLimit when
// raw is empty, and clamping any value above MaxAPILimit down to it. Returns an error
// for a non-integer or non-positive value - both are "malformed", so callers respond
// 400 rather than silently substituting the default.
func parseAPILimit(raw string, defaultLimit int) (int, error) {
	if raw == "" {
		return defaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, strconv.ErrRange
	}
	if n > MaxAPILimit {
		n = MaxAPILimit
	}
	return n, nil
}

// ---- GET /api/blocks, GET /api/blocks/{height} ----

// handleAPIBlocks serves GET /api/blocks: a JSON array of db.Block rows, the same
// before-cursor + limit pagination shape handleBlocksList/handleBlocksPartial (GET /
// and GET /blocks/partial) already use (s.DB.ListBlocks) - a JSON caller doesn't need
// the HTML routes' "full page vs HTMX rows-only partial" split, so this collapses both
// into one endpoint. ?before=<height> defaults to math.MaxInt64 (first page) when
// omitted; ?limit=<n> defaults to PageSize (25), clamped to MaxAPILimit (100).
func (s *Server) handleAPIBlocks(w http.ResponseWriter, r *http.Request) {
	before := int64(math.MaxInt64)
	if raw := r.URL.Query().Get("before"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid before parameter")
			return
		}
		before = v
	}
	limit, err := parseAPILimit(r.URL.Query().Get("limit"), PageSize)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid limit parameter")
		return
	}

	blocks, err := s.DB.ListBlocks(r.Context(), before, limit)
	if err != nil {
		log.Printf("server: api: list blocks: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to load blocks")
		return
	}
	if blocks == nil {
		blocks = []db.Block{}
	}
	writeJSON(w, http.StatusOK, blocks)
}

// apiBlockDetail is the GET /api/blocks/{height} response body: the raw db.Block plus
// its per-kernel/per-output breakdown, mirroring handleBlockDetail's (GET
// /blocks/{height}) three DB calls (s.DB.GetBlock/GetKernelsForBlock/
// GetOutputsForBlock) exactly.
type apiBlockDetail struct {
	Block   db.Block    `json:"block"`
	Kernels []db.Kernel `json:"kernels"`
	Outputs []db.Output `json:"outputs"`
}

// handleAPIBlockDetail serves GET /api/blocks/{height}: see apiBlockDetail's doc
// comment. Responds 404 (JSON error body) when the height doesn't exist, the same
// errors.Is(err, pgx.ErrNoRows) check handleBlockDetail already uses.
func (s *Server) handleAPIBlockDetail(w http.ResponseWriter, r *http.Request) {
	heightStr := r.PathValue("height")
	height, err := strconv.ParseUint(heightStr, 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid block height")
		return
	}

	block, err := s.DB.GetBlock(r.Context(), height)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, "block not found")
			return
		}
		log.Printf("server: api: get block %d: %v", height, err)
		writeJSONError(w, http.StatusInternalServerError, "failed to load block")
		return
	}

	kernels, err := s.DB.GetKernelsForBlock(r.Context(), height)
	if err != nil {
		log.Printf("server: api: get kernels for block %d: %v", height, err)
		writeJSONError(w, http.StatusInternalServerError, "failed to load kernels")
		return
	}
	outputs, err := s.DB.GetOutputsForBlock(r.Context(), height)
	if err != nil {
		log.Printf("server: api: get outputs for block %d: %v", height, err)
		writeJSONError(w, http.StatusInternalServerError, "failed to load outputs")
		return
	}
	if kernels == nil {
		kernels = []db.Kernel{}
	}
	if outputs == nil {
		outputs = []db.Output{}
	}

	writeJSON(w, http.StatusOK, apiBlockDetail{Block: block, Kernels: kernels, Outputs: outputs})
}

// ---- GET /api/pool-stats ----

// handleAPIPoolStats serves GET /api/pool-stats: the raw poolstats.PoolStats struct,
// mirroring handlePoolStats' (GET /pool-stats) s.PoolStats.GetStats(ctx) call. Unlike
// the HTML page (which degrades to an inline error message rather than a non-200
// status - a pool-backend outage isn't a server error there), a JSON API consumer
// needs an explicit signal: an unconfigured s.PoolStats or a failed fetch both respond
// 503 Service Unavailable with a JSON error body rather than degrading silently.
func (s *Server) handleAPIPoolStats(w http.ResponseWriter, r *http.Request) {
	if s.PoolStats == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "pool stats provider not configured")
		return
	}
	stats, err := s.PoolStats.GetStats(r.Context())
	if err != nil {
		log.Printf("server: api: get pool stats: %v", err)
		writeJSONError(w, http.StatusServiceUnavailable, "unable to reach pool stats backend")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// ---- GET /api/analysis/* ----
//
// Each of the 7 routes below is a separate handler (rather than one
// GET /api/analysis/{view} route) mirroring analysis.go's existing per-view HTML
// handler structure (handleAnalysisAlgoDistribution, handleAnalysisPoolShare, etc.) 1:1
// - that file already has one handler function per view rather than a single
// dispatch-on-{view} handler, so this JSON namespace follows the same convention
// instead of introducing a second, inconsistent routing style alongside it.
//
// Each reuses s.parseAnalysisParams (the same bucket_size/from/to query-param parsing
// analysis.go's HTML handlers already use - not duplicated here) and calls the
// underlying internal/db bucketed-aggregation method DIRECTLY (db.AlgoBucketCounts,
// db.PoolShareBucketCounts, etc.) rather than going through internal/analysis's
// chartrender.Point-shaped wrapper functions (analysis.AlgoDistribution, etc.): those
// exist purely to feed chartrender's PNG rendering and the HTML data-table view, and
// would require inventing a new JSON shape on top of an already-existing, more
// directly useful one. Each response is a bare JSON array of that db method's own
// row type (already JSON-tagged - see internal/db's AlgoBucketRow/PoolShareBucketRow/
// BlockTimeBucketRow/DifficultyBucketRow/RewardBucketRow/RewardPoolBucketRow doc
// comments), per this endpoint family's "raw struct, no bespoke DTO" convention -
// never a PNG, and never null (an empty result marshals as `[]`).

// handleAPIAnalysisAlgoDistribution serves GET /api/analysis/algo-distribution: a JSON
// array of db.AlgoBucketRow, mirroring handleAnalysisAlgoDistribution's
// s.DB.AlgoBucketCounts call.
func (s *Server) handleAPIAnalysisAlgoDistribution(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	rows, err := s.DB.AlgoBucketCounts(r.Context(), p.BucketSize, p.From, p.To)
	if err != nil {
		log.Printf("server: api: analysis algo distribution: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "unable to load chart data")
		return
	}
	if rows == nil {
		rows = []db.AlgoBucketRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleAPIAnalysisPoolShare serves GET /api/analysis/pool-share: a JSON array of
// db.PoolShareBucketRow, mirroring handleAnalysisPoolShare's
// s.DB.PoolShareBucketCounts call (same analysis.DefaultTopPools/
// analysis.DefaultPoolTagMappings defaults).
func (s *Server) handleAPIAnalysisPoolShare(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	rows, err := s.DB.PoolShareBucketCounts(r.Context(), p.BucketSize, p.From, p.To, analysis.DefaultTopPools, analysis.DefaultPoolTagMappings)
	if err != nil {
		log.Printf("server: api: analysis pool share: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "unable to load chart data")
		return
	}
	if rows == nil {
		rows = []db.PoolShareBucketRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleAPIAnalysisPoolAlgoBreakdown serves GET /api/analysis/pool-algo-breakdown: a
// JSON array of db.AlgoBucketRow scoped to a single pool, mirroring
// handleAnalysisPoolAlgoBreakdown's s.DB.AlgoBucketCountsForPool call. ?pool= follows
// the exact same semantics as the HTML view (parsePoolAlgoBreakdownPool, reused as-is):
// defaults to the first entry in analysis.DefaultPoolTagMappings when absent/blank. If
// no pool can be resolved (absent AND no default mapping configured), responds 400
// with a JSON error rather than querying with an empty pool value.
func (s *Server) handleAPIAnalysisPoolAlgoBreakdown(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	pool := parsePoolAlgoBreakdownPool(r)
	if pool == "" {
		writeJSONError(w, http.StatusBadRequest, "no pool specified and no default pool tag mapping configured")
		return
	}
	rows, err := s.DB.AlgoBucketCountsForPool(r.Context(), p.BucketSize, p.From, p.To, analysis.DefaultPoolTagMappings, pool)
	if err != nil {
		log.Printf("server: api: analysis pool algo breakdown: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "unable to load chart data")
		return
	}
	if rows == nil {
		rows = []db.AlgoBucketRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleAPIAnalysisBlockTime serves GET /api/analysis/block-time: a JSON array of
// db.BlockTimeBucketRow, mirroring handleAnalysisBlockTime's own second, explicit
// s.DB.BlockTimeDeltaBuckets call (the same one that feeds that HTML view's per-bucket
// data table) rather than analysis.BlockTime's chart-shaped Points/summary return.
func (s *Server) handleAPIAnalysisBlockTime(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	rows, err := s.DB.BlockTimeDeltaBuckets(r.Context(), p.BucketSize, p.From, p.To)
	if err != nil {
		log.Printf("server: api: analysis block time: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "unable to load chart data")
		return
	}
	if rows == nil {
		rows = []db.BlockTimeBucketRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleAPIAnalysisDifficulty serves GET /api/analysis/difficulty: a JSON array of
// db.DifficultyBucketRow, mirroring handleAnalysisDifficulty's s.DB.DifficultyBucketAvg
// call. Per-algo fields are nil (JSON null) for a bucket/algo combination with zero
// blocks - see DifficultyBucketRow's own doc comment for why that must never be
// coerced to 0.0.
func (s *Server) handleAPIAnalysisDifficulty(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	rows, err := s.DB.DifficultyBucketAvg(r.Context(), p.BucketSize, p.From, p.To)
	if err != nil {
		log.Printf("server: api: analysis difficulty: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "unable to load chart data")
		return
	}
	if rows == nil {
		rows = []db.DifficultyBucketRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleAPIAnalysisRewardsByAlgo serves GET /api/analysis/rewards-by-algo: a JSON
// array of db.RewardBucketRow (raw MicroMinotari sums, see that type's doc comment -
// no XTM/formatMicroMinotari conversion here, unlike the HTML table's
// newRewardsTableView), mirroring handleAnalysisRewardsByAlgo's
// s.DB.RewardBucketCountsByAlgo call.
func (s *Server) handleAPIAnalysisRewardsByAlgo(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	rows, err := s.DB.RewardBucketCountsByAlgo(r.Context(), p.BucketSize, p.From, p.To)
	if err != nil {
		log.Printf("server: api: analysis rewards by algo: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "unable to load chart data")
		return
	}
	if rows == nil {
		rows = []db.RewardBucketRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleAPIAnalysisRewardsByPool serves GET /api/analysis/rewards-by-pool: a JSON
// array of db.RewardPoolBucketRow (raw MicroMinotari sums, same no-XTM-conversion
// rationale as handleAPIAnalysisRewardsByAlgo above), mirroring
// handleAnalysisRewardsByPool's s.DB.RewardBucketCountsByPool call (same
// analysis.DefaultTopPools/analysis.DefaultPoolTagMappings defaults).
func (s *Server) handleAPIAnalysisRewardsByPool(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	rows, err := s.DB.RewardBucketCountsByPool(r.Context(), p.BucketSize, p.From, p.To, analysis.DefaultTopPools, analysis.DefaultPoolTagMappings)
	if err != nil {
		log.Printf("server: api: analysis rewards by pool: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "unable to load chart data")
		return
	}
	if rows == nil {
		rows = []db.RewardPoolBucketRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// ---- GET /api/tip-info ----

// apiTipInfo is the GET /api/tip-info response body, mirroring the real
// tari_generated.TipInfoResponse/MetaData fields returned by
// s.Node.GetTipInfo(ctx) - verified via `go doc
// github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated.TipInfoResponse` and
// `... .MetaData` against this module's actual go-tari-grpc-lib/v3 dependency (NOT
// guessed from textexplore.tari.com's own JSON sample):
//
//	TipInfoResponse{ Metadata *MetaData; InitialSyncAchieved bool; BaseNodeState BaseNodeState; FailedCheckpoints bool }
//	MetaData{ BestBlockHeight uint64; BestBlockHash []byte; AccumulatedDifficulty []byte; PrunedHeight uint64; Timestamp uint64 }
//
// BestBlockHash/AccumulatedDifficulty are hex-encoded strings here (not base64),
// matching this repo's own existing byte-rendering convention for hash-shaped values
// (kernelView.ExcessSigHex's fmt.Sprintf("%x%x", ...) pattern, block.Hash/PrevHash
// already being stored as hex strings - see db.Block's doc comment) - unlike
// db.Block/db.Kernel/db.Output's own secondary merkle-root/offset []byte fields
// (deliberately left as default-base64 raw struct fields, see those types' doc
// comments), BestBlockHash/AccumulatedDifficulty ARE exactly this kind of primary,
// human-meaningful hash identifier, so this DTO (built by hand rather than a JSON-
// tagged proto struct) hex-encodes them. BaseNodeState is rendered as its human enum
// name (BaseNodeState.String(), e.g. "LISTENING") rather than its raw int32 wire value,
// for the same "don't make a JSON consumer memorize a magic number" reasoning.
type apiTipInfo struct {
	BestBlockHeight       uint64 `json:"best_block_height"`
	BestBlockHash         string `json:"best_block_hash"`
	AccumulatedDifficulty string `json:"accumulated_difficulty"`
	PrunedHeight          uint64 `json:"pruned_height"`
	Timestamp             uint64 `json:"timestamp"`
	InitialSyncAchieved   bool   `json:"initial_sync_achieved"`
	BaseNodeState         string `json:"base_node_state"`
	FailedCheckpoints     bool   `json:"failed_checkpoints"`
}

// handleAPITipInfo serves GET /api/tip-info: this repo's real analog to
// textexplore.tari.com's own `tipInfo` field, since (unlike the L2/Ootle sibling repo)
// this repo has a real base-node GRPC connection to ask - see apiTipInfo's doc comment
// for the field-by-field derivation. Degrades to a 503 JSON error (never a panic) when
// s.Node is nil (no base-node GRPC host configured, same nil-safety convention every
// other s.Node-using handler in this package follows) or when the live GetTipInfo call
// itself fails (e.g. every configured host unreachable) - a JSON API consumer needs an
// explicit signal for either case, same rationale as handleAPIPoolStats above.
func (s *Server) handleAPITipInfo(w http.ResponseWriter, r *http.Request) {
	if s.Node == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "tip info unavailable: no base-node GRPC host configured")
		return
	}
	resp, err := s.Node.GetTipInfo(r.Context())
	if err != nil {
		log.Printf("server: api: tip info: %v", err)
		writeJSONError(w, http.StatusServiceUnavailable, "tip info unavailable")
		return
	}

	md := resp.GetMetadata()
	writeJSON(w, http.StatusOK, apiTipInfo{
		BestBlockHeight:       md.GetBestBlockHeight(),
		BestBlockHash:         fmt.Sprintf("%x", md.GetBestBlockHash()),
		AccumulatedDifficulty: fmt.Sprintf("%x", md.GetAccumulatedDifficulty()),
		PrunedHeight:          md.GetPrunedHeight(),
		Timestamp:             md.GetTimestamp(),
		InitialSyncAchieved:   resp.GetInitialSyncAchieved(),
		BaseNodeState:         resp.GetBaseNodeState().String(),
		FailedCheckpoints:     resp.GetFailedCheckpoints(),
	})
}

// ---- GET /api/health ----

// handleAPIHealth serves GET /api/health: a cheap "is this HTTP process up and is its
// Postgres connection queryable" liveness check - this repo has no existing /health
// route to mirror (confirmed by reading server.go's Handler() route list in full), so
// this follows the sibling go-tari-ootle-explorer repo's own /api/health convention
// instead: s.DB.MaxIndexedHeight(ctx) succeeding is the "database reachable" signal,
// and the response is ALWAYS 200 - a degraded database is reported in the body
// ("database": "degraded" plus an "error" detail), never via a non-200 status. This is
// deliberately a database-only check, not a base-node/pool-stats reachability probe:
// those are optional dependencies (s.Node/s.PoolStats may be legitimately nil) whose
// absence isn't this process being unhealthy.
func (s *Server) handleAPIHealth(w http.ResponseWriter, r *http.Request) {
	if _, err := s.DB.MaxIndexedHeight(r.Context()); err != nil {
		writeJSON(w, http.StatusOK, map[string]string{
			"status":   "ok",
			"database": "degraded",
			"error":    err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":   "ok",
		"database": "reachable",
	})
}
