// Package analysis is the glue layer for the historical-analysis feature area: it calls
// the bucketed query methods on internal/db (AlgoBucketCounts, PoolShareBucketCounts,
// BlockTimeDeltaBuckets, BlockTimeSummary, DifficultyBucketAvg) and reshapes their
// db-specific row types into the plain chartrender.Point/series-name shape that
// internal/chartrender renders into PNG bytes, and that internal/server's analysis
// handlers embed via <img> tags. Keeping this reshaping here (rather than in
// internal/server or internal/chartrender) is what lets chartrender stay entirely
// decoupled from internal/db's types and lets the server handlers stay thin
// query-params-in/template-out glue.
package analysis

import (
	"context"
	"fmt"
	"sort"

	"github.com/Snipa22/go-tari-explorer/internal/chartrender"
	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// DefaultBucketSize is used by internal/server's analysis handlers when the
// bucket_size query parameter is absent, matching cmd/algobuckets' own default.
const DefaultBucketSize = 1000

// DefaultTopPools caps the number of distinct pool_tag series the pool-share chart draws
// before folding the remainder into "other" (see db.PoolShareBucketCounts). 8 is picked
// as a legend size that still comfortably fits under a ~960px-wide chart without
// wrapping onto many lines or shrinking to unreadable text, while still surfacing the
// handful of pools that plausibly matter on a real Tari RandomX/SHA3X pool landscape;
// the long tail beyond that is exactly what "other" exists to absorb.
const DefaultTopPools = 8

// DefaultPoolTagMappings is the "known tag-family -> canonical display name" mapping
// (see db.PoolTagMapping) used by the pool-share chart and the per-pool algo-breakdown
// view to fold a single pool operator's per-node/per-worker pool_tag values into one
// display series, rather than one series per physical node.
//
// This is a general, data-driven mechanism, not a one-off special case for the repo
// owner's own pool: any other pool operator whose pool_tag values fragment the same
// way (shared prefix, per-node/per-worker suffix) can be added here as one more
// {MatchPrefix, CanonicalName} entry - no code changes required in db/analysis/server.
//
// The single entry below (WUF -> "Jagtech") is the repo owner's own pool
// infrastructure. Verified directly against live production data (query:
// `SELECT DISTINCT pool_tag FROM blocks WHERE pool_tag LIKE 'WUF%'` against the
// tari_explorer database, 2026-08-23) that the WUF prefix covers every currently
// fragmented pool_tag family in the table (active node family "Jagtech", legacy/
// inactive node names "Ahri"/"Nytro"/"Taila"/"Ara-Ayn"/"Nia-Mio"/"Stratum"/
// "Graha'tia"/"Y'shtola") - all of it the same pool operator's infrastructure sharing
// the "WUF" coinbase-extra prefix - and that no other pool currently listed in
// prefixTable (e.g. pool.kryptex.com) exhibits this problem, so it needs no entry here.
var DefaultPoolTagMappings = []db.PoolTagMapping{
	{MatchPrefix: "WUF", CanonicalName: "Jagtech"},
}

// AlgoOrder is the fixed stack/legend order used for the algo-distribution chart,
// matching db.AlgoBucketRow's field order (and cmd/algobuckets' table column order).
var AlgoOrder = []string{"RXM", "RXT", "C29", "SHA3X"}

// AlgoDistribution loads db.AlgoBucketCounts for [fromHeight, toHeight] and reshapes it
// into chartrender.Points (one per bucket, one Series entry per pow-algo) ready for
// chartrender.StackedAreaChart, plus the series order/name list to pass alongside it.
func AlgoDistribution(ctx context.Context, database *db.DB, bucketSize, fromHeight, toHeight uint64) ([]chartrender.Point, []string, error) {
	rows, err := database.AlgoBucketCounts(ctx, bucketSize, fromHeight, toHeight)
	if err != nil {
		return nil, nil, fmt.Errorf("analysis: algo distribution: %w", err)
	}
	points := make([]chartrender.Point, len(rows))
	for i, r := range rows {
		points[i] = chartrender.Point{
			X: float64(r.BucketStart),
			Series: map[string]float64{
				"RXM":   float64(r.RXM),
				"RXT":   float64(r.RXT),
				"C29":   float64(r.C29),
				"SHA3X": float64(r.SHA3X),
			},
		}
	}
	return points, AlgoOrder, nil
}

// PoolOptions builds the full ?pool= choice list for the pool-algo-breakdown view's
// dropdown: every mappings[i].CanonicalName (the folded, display-ready pool series
// names - see db.PoolTagMapping / DefaultPoolTagMappings) plus every real pool_tag
// currently stored in `blocks` that doesn't match any mapping's MatchPrefix (see
// db.DB.UnmappedPoolTags) - pool operators who haven't (yet) been folded into a
// canonical mapping, whose raw pool_tag is nonetheless just as valid a literal ?pool=
// value per db.AlgoBucketCountsForPool's doc comment. The combined set is deduped and
// sorted alphabetically, so the dropdown has a stable order independent of mapping
// declaration order or database row order.
func PoolOptions(ctx context.Context, database *db.DB, mappings []db.PoolTagMapping) ([]string, error) {
	unmapped, err := database.UnmappedPoolTags(ctx, mappings)
	if err != nil {
		return nil, fmt.Errorf("analysis: pool options: %w", err)
	}

	seen := make(map[string]struct{}, len(mappings)+len(unmapped))
	var out []string
	for _, m := range mappings {
		if _, ok := seen[m.CanonicalName]; ok {
			continue
		}
		seen[m.CanonicalName] = struct{}{}
		out = append(out, m.CanonicalName)
	}
	for _, tag := range unmapped {
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	sort.Strings(out)
	return out, nil
}

// PoolShare loads db.PoolShareBucketCounts for [fromHeight, toHeight] (capped to topN
// distinct mapped pool tags, see db.PoolShareBucketCounts) and reshapes its "long" row
// format (one row per bucket+pool combination) into chartrender.Points (one per bucket,
// one Series entry per pool key) plus a deterministic series order for
// chartrender.StackedAreaChart: pool keys ordered by total block count across the
// queried range descending, with "unknown" and "other" always sorted last (in that
// order) regardless of their totals, so the "real pool" bands stack together below the
// catch-all bands at the top rather than an arbitrary/unstable interleaving.
//
// mappings folds known tag families (e.g. every WUFJagtech*/WUF  Ahri   -shaped tag)
// into one canonical series before the topN/ordering logic runs - see
// db.PoolTagMapping and DefaultPoolTagMappings. Pass nil to disable folding entirely.
func PoolShare(ctx context.Context, database *db.DB, bucketSize, fromHeight, toHeight uint64, topN int, mappings []db.PoolTagMapping) ([]chartrender.Point, []string, error) {
	rows, err := database.PoolShareBucketCounts(ctx, bucketSize, fromHeight, toHeight, topN, mappings)
	if err != nil {
		return nil, nil, fmt.Errorf("analysis: pool share: %w", err)
	}

	pointsByBucket := map[uint64]map[string]float64{}
	var bucketOrder []uint64
	totals := map[string]int64{}
	for _, r := range rows {
		if _, ok := pointsByBucket[r.BucketStart]; !ok {
			pointsByBucket[r.BucketStart] = map[string]float64{}
			bucketOrder = append(bucketOrder, r.BucketStart)
		}
		pointsByBucket[r.BucketStart][r.PoolTag] += float64(r.Count)
		totals[r.PoolTag] += r.Count
	}

	seriesOrder := poolSeriesOrder(totals)

	points := make([]chartrender.Point, len(bucketOrder))
	for i, bucket := range bucketOrder {
		points[i] = chartrender.Point{X: float64(bucket), Series: pointsByBucket[bucket]}
	}
	return points, seriesOrder, nil
}

// poolSeriesOrder orders pool keys by descending total block count, with the "unknown"
// and "other" catch-all keys always sorted last (in that order) regardless of their
// totals - see PoolShare's doc comment for why.
func poolSeriesOrder(totals map[string]int64) []string {
	var real []string
	for key := range totals {
		if key == "unknown" || key == "other" {
			continue
		}
		real = append(real, key)
	}
	// Simple insertion sort by descending total, stable on key name for equal totals -
	// the series count here is bounded by topN (single digits in practice), so O(n^2)
	// is irrelevant.
	for i := 1; i < len(real); i++ {
		for j := i; j > 0 && (totals[real[j]] > totals[real[j-1]] ||
			(totals[real[j]] == totals[real[j-1]] && real[j] < real[j-1])); j-- {
			real[j], real[j-1] = real[j-1], real[j]
		}
	}
	out := real
	if _, ok := totals["unknown"]; ok {
		out = append(out, "unknown")
	}
	if _, ok := totals["other"]; ok {
		out = append(out, "other")
	}
	return out
}

// BlockTime loads db.BlockTimeDeltaBuckets for [fromHeight, toHeight] and reshapes it
// into chartrender.Points (one per bucket, single "block time (s)" series holding the
// bucket's median inter-block time) for chartrender.LineChart, plus the accompanying
// db.BlockTimeSummaryRow (mean/median/stddev/max/sample count) for the plain-HTML
// stat-card panel. Buckets with zero usable samples (MedianSeconds == nil, e.g. every
// block in that bucket had a missing predecessor) are included as a zero-value point
// rather than dropped, so the chart's X axis still spans the full requested range.
const blockTimeSeriesName = "block time (s)"

func BlockTime(ctx context.Context, database *db.DB, bucketSize, fromHeight, toHeight uint64) ([]chartrender.Point, db.BlockTimeSummaryRow, error) {
	rows, err := database.BlockTimeDeltaBuckets(ctx, bucketSize, fromHeight, toHeight)
	if err != nil {
		return nil, db.BlockTimeSummaryRow{}, fmt.Errorf("analysis: block time: %w", err)
	}
	summary, err := database.BlockTimeSummary(ctx, fromHeight, toHeight)
	if err != nil {
		return nil, db.BlockTimeSummaryRow{}, fmt.Errorf("analysis: block time: %w", err)
	}

	points := make([]chartrender.Point, len(rows))
	for i, r := range rows {
		var v float64
		if r.MedianSeconds != nil {
			v = *r.MedianSeconds
		}
		points[i] = chartrender.Point{X: float64(r.BucketStart), Series: map[string]float64{blockTimeSeriesName: v}}
	}
	return points, summary, nil
}

// DifficultyOrder is the single-series name used for the difficulty chart.
const difficultySeriesName = "avg difficulty"

// Difficulty loads db.DifficultyBucketAvg for [fromHeight, toHeight] and reshapes it into
// chartrender.Points (one per bucket, single "avg difficulty" series) for
// chartrender.LineChart. See db.DifficultyBucketRow's doc comment for why this is a
// difficulty chart rather than a literal hashrate chart.
func Difficulty(ctx context.Context, database *db.DB, bucketSize, fromHeight, toHeight uint64) ([]chartrender.Point, string, error) {
	rows, err := database.DifficultyBucketAvg(ctx, bucketSize, fromHeight, toHeight)
	if err != nil {
		return nil, "", fmt.Errorf("analysis: difficulty: %w", err)
	}
	points := make([]chartrender.Point, len(rows))
	for i, r := range rows {
		points[i] = chartrender.Point{X: float64(r.BucketStart), Series: map[string]float64{difficultySeriesName: r.AvgDifficulty}}
	}
	return points, difficultySeriesName, nil
}

// PoolAlgoBreakdown loads db.AlgoBucketCountsForPool for [fromHeight, toHeight], scoped
// to canonicalName via mappings (see db.PoolTagMapping / db.AlgoBucketCountsForPool),
// and reshapes it into chartrender.Points using the same RXM/RXT/C29/SHA3X series shape
// as AlgoDistribution - this is AlgoDistribution filtered down to one pool operator's
// merged tag family, showing which algo(s) that operator's nodes actually mine.
//
// This is a general mechanism, not hardcoded to any one pool: canonicalName is any
// name present in mappings' CanonicalName fields (or, if absent from mappings, treated
// as a literal unmapped pool_tag - see db.AlgoBucketCountsForPool), so it works for
// the repo owner's own "Jagtech" series today and for any other pool operator's merged
// series added to the mapping later.
func PoolAlgoBreakdown(ctx context.Context, database *db.DB, bucketSize, fromHeight, toHeight uint64, mappings []db.PoolTagMapping, canonicalName string) ([]chartrender.Point, []string, error) {
	rows, err := database.AlgoBucketCountsForPool(ctx, bucketSize, fromHeight, toHeight, mappings, canonicalName)
	if err != nil {
		return nil, nil, fmt.Errorf("analysis: pool algo breakdown: %w", err)
	}
	points := make([]chartrender.Point, len(rows))
	for i, r := range rows {
		points[i] = chartrender.Point{
			X: float64(r.BucketStart),
			Series: map[string]float64{
				"RXM":   float64(r.RXM),
				"RXT":   float64(r.RXT),
				"C29":   float64(r.C29),
				"SHA3X": float64(r.SHA3X),
			},
		}
	}
	return points, AlgoOrder, nil
}
