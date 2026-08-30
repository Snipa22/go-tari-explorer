// Historical-analysis HTTP routes: a landing page plus four analysis views
// (algo-distribution, pool-share, block-time, difficulty), each an HTML page embedding
// a server-rendered PNG chart (via internal/chartrender, fed by internal/analysis) as
// an <img> tag - no client-side JS charting. Each HTML page and its companion .png
// endpoint share the same bucket_size/from/to query-param parsing so the <img src>
// resubmits exactly the params the page itself was loaded with.
package server

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"

	"github.com/Snipa22/go-tari-explorer/internal/analysis"
	"github.com/Snipa22/go-tari-explorer/internal/chartrender"
	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// analysisParams is the parsed/defaulted bucket_size/from/to query-param triple shared
// by every analysis HTML page and PNG endpoint.
type analysisParams struct {
	BucketSize uint64
	From       uint64
	To         uint64
}

// String renders the params back into a query string, used by the HTML templates to
// build each page's <img src="...png?..."> and form-resubmit action consistently.
func (p analysisParams) String() string {
	return "bucket_size=" + strconv.FormatUint(p.BucketSize, 10) +
		"&from=" + strconv.FormatUint(p.From, 10) +
		"&to=" + strconv.FormatUint(p.To, 10)
}

// defaultAnalysisToHeight is used as the "to" bound when the caller doesn't specify one
// and the DB has no indexed blocks yet (MaxIndexedHeight returns 0) - large enough that
// a real range query still returns real results, matching cmd/algobuckets' own
// zero-args behavior.
const defaultAnalysisToHeight = 1_000_000

// defaultAnalysisWindowBlocks is how many blocks back from the highest currently-
// indexed height the "from" default covers when the caller doesn't specify ?from= -
// full-history charts get visually unreadable as the chain grows, so the default view
// is a recent window instead. Explicit ?from= still overrides this completely.
const defaultAnalysisWindowBlocks = 10_000

// parseAnalysisParams reads bucket_size/from/to from the request's query string,
// defaulting bucket_size to analysis.DefaultBucketSize, from to
// max(0, highest-indexed-height - defaultAnalysisWindowBlocks) (i.e. the last ~10,000
// indexed blocks, or 0 if the table is empty/errored/shorter than that window), and to
// to the highest currently-indexed block height (or defaultAnalysisToHeight if the
// table is empty). Malformed numeric params are silently treated as absent (fall back
// to the default) rather than producing a 400 - this is a read-only reporting page,
// not a form that mutates state, so a bad param degrading gracefully is preferable to
// erroring.
func (s *Server) parseAnalysisParams(r *http.Request) analysisParams {
	q := r.URL.Query()
	p := analysisParams{BucketSize: analysis.DefaultBucketSize}
	if v, err := strconv.ParseUint(q.Get("bucket_size"), 10, 64); err == nil && v > 0 {
		p.BucketSize = v
	}

	fromV, fromErr := strconv.ParseUint(q.Get("from"), 10, 64)
	fromAbsent := fromErr != nil
	toV, toErr := strconv.ParseUint(q.Get("to"), 10, 64)
	toAbsent := toErr != nil || toV == 0

	if !fromAbsent {
		p.From = fromV
	}

	if toAbsent {
		// Common case: whether or not from is also absent, we need MaxIndexedHeight
		// for to's default here - so from's default (when also absent) can share
		// this exact call instead of querying a second time.
		max, err := s.DB.MaxIndexedHeight(r.Context())
		if err != nil || max == 0 {
			p.To = defaultAnalysisToHeight
			if fromAbsent {
				p.From = 0
			}
		} else {
			p.To = max
			if fromAbsent {
				p.From = defaultAnalysisFrom(max)
			}
		}
	} else {
		p.To = toV
		if fromAbsent {
			// to was explicit, so the above branch's shared MaxIndexedHeight call
			// didn't happen - from's default still needs its own call here.
			max, err := s.DB.MaxIndexedHeight(r.Context())
			if err != nil || max == 0 {
				p.From = 0
			} else {
				p.From = defaultAnalysisFrom(max)
			}
		}
	}

	return p
}

// defaultAnalysisFrom computes the default "from" height given the highest currently-
// indexed block height: max(0, maxHeight - defaultAnalysisWindowBlocks), avoiding
// uint64 underflow when maxHeight is less than the window size.
func defaultAnalysisFrom(maxHeight uint64) uint64 {
	if maxHeight < defaultAnalysisWindowBlocks {
		return 0
	}
	return maxHeight - defaultAnalysisWindowBlocks
}

// handleAnalysisIndex serves the /analysis landing page: links to the 4 views.
func (s *Server) handleAnalysisIndex(w http.ResponseWriter, r *http.Request) {
	if err := s.analysisIndexTmpl.Execute(w, nil); err != nil {
		log.Printf("server: render analysis index: %v", err)
	}
}

// imgSrc builds the `<img src>` value for a given analysis .png endpoint and the
// current request's params, as a template.URL so html/template's contextual
// autoescaper treats the query-string separators (&, =) literally instead of
// percent-encoding them (which would otherwise produce a src the server-side query
// parser can't split back into individual params).
func imgSrc(pngPath string, p analysisParams) template.URL {
	return template.URL(pngPath + "?" + p.String())
}

// analysisViewData is the template data shape shared by all 4 analysis HTML view pages.
type analysisViewData struct {
	Title      string
	ImgSrc     template.URL
	Params     analysisParams
	Error      string
	Summary    *blockTimeSummaryView
	Difficulty bool
	// Table is the per-bucket data table shown below the chart, populated only on
	// success (left nil on error so the template's existing {{if .Error}} branch
	// continues to work unchanged).
	Table *analysisTableView
	// PoolField/Pool/PoolOptions are set only by the pool-algo-breakdown view, which
	// needs an extra "pool" query param the other 4 analysis views don't have. Zero
	// value (PoolField == false) hides the extra form field for every other view.
	// PoolOptions is the data-driven dropdown choice list built by
	// analysis.PoolOptions - see handleAnalysisPoolAlgoBreakdown.
	PoolField   bool
	Pool        string
	PoolOptions []string
}

// blockTimeSummaryView adapts db.BlockTimeSummaryRow for template rendering, pre-
// formatting its nullable pointer fields into display strings ("n/a" when nil) so the
// template itself never needs to dereference a pointer.
type blockTimeSummaryView struct {
	Mean        string
	Median      string
	StdDev      string
	Max         string
	SampleCount int64
}

func formatSecondsPtr(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return strconv.FormatFloat(*v, 'f', 2, 64)
}

func newBlockTimeSummaryView(r db.BlockTimeSummaryRow) blockTimeSummaryView {
	maxStr := "n/a"
	if r.Max != nil {
		maxStr = strconv.FormatInt(*r.Max, 10)
	}
	return blockTimeSummaryView{
		Mean:        formatSecondsPtr(r.Mean),
		Median:      formatSecondsPtr(r.Median),
		StdDev:      formatSecondsPtr(r.StdDev),
		Max:         maxStr,
		SampleCount: r.SampleCount,
	}
}

func (s *Server) handleAnalysisAlgoDistribution(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	data := analysisViewData{Title: "Algo Distribution", ImgSrc: imgSrc("/analysis/algo-distribution.png", p), Params: p}
	// tooFewBucketsForChart is intentionally not checked here: that shape is not an
	// error (the .png endpoint renders a graceful placeholder for it - see
	// tooFewBucketsForChart's doc comment), so the HTML page renders normally too.
	if points, order, err := analysis.AlgoDistribution(r.Context(), s.DB, p.BucketSize, p.From, p.To); err != nil {
		log.Printf("server: analysis algo distribution: %v", err)
		data.Error = "unable to load chart data"
	} else {
		data.Table = newAnalysisTableView(points, order, true)
	}
	if err := s.analysisViewTmpl.Execute(w, data); err != nil {
		log.Printf("server: render analysis algo distribution: %v", err)
	}
}

func (s *Server) handleAnalysisPoolShare(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	data := analysisViewData{Title: "Pool Share", ImgSrc: imgSrc("/analysis/pool-share.png", p), Params: p}
	// See handleAnalysisAlgoDistribution's comment above re: tooFewBucketsForChart.
	if points, order, err := analysis.PoolShare(r.Context(), s.DB, p.BucketSize, p.From, p.To, analysis.DefaultTopPools, analysis.DefaultPoolTagMappings); err != nil {
		log.Printf("server: analysis pool share: %v", err)
		data.Error = "unable to load chart data"
	} else {
		data.Table = newAnalysisTableView(points, order, true)
	}
	if err := s.analysisViewTmpl.Execute(w, data); err != nil {
		log.Printf("server: render analysis pool share: %v", err)
	}
}

func (s *Server) handleAnalysisBlockTime(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	data := analysisViewData{Title: "Block Time", ImgSrc: imgSrc("/analysis/block-time.png", p), Params: p}
	// See handleAnalysisAlgoDistribution's comment above re: tooFewBucketsForChart.
	points, summary, err := analysis.BlockTime(r.Context(), s.DB, p.BucketSize, p.From, p.To)
	if err != nil {
		log.Printf("server: analysis block time: %v", err)
		data.Error = "unable to load chart data"
	} else {
		v := newBlockTimeSummaryView(summary)
		data.Summary = &v
		// analysis.BlockTime has no []string order return - the series is
		// implicitly named "block time (s)", matching the PNG's own
		// chartrender.LineChart call below in handleAnalysisBlockTimePNG.
		data.Table = newAnalysisTableView(points, []string{"block time (s)"}, false)
	}
	if err := s.analysisViewTmpl.Execute(w, data); err != nil {
		log.Printf("server: render analysis block time: %v", err)
	}
}

func (s *Server) handleAnalysisDifficulty(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	data := analysisViewData{Title: "Difficulty", ImgSrc: imgSrc("/analysis/difficulty.png", p), Params: p, Difficulty: true}
	// See handleAnalysisAlgoDistribution's comment above re: tooFewBucketsForChart.
	if points, order, err := analysis.Difficulty(r.Context(), s.DB, p.BucketSize, p.From, p.To); err != nil {
		log.Printf("server: analysis difficulty: %v", err)
		data.Error = "unable to load chart data"
	} else {
		data.Table = newAnalysisTableView(points, order, false)
	}
	if err := s.analysisViewTmpl.Execute(w, data); err != nil {
		log.Printf("server: render analysis difficulty: %v", err)
	}
}

// defaultPoolAlgoBreakdownPool is the canonical pool name the /analysis/pool-algo-
// breakdown view/PNG default to when no ?pool= is supplied: the first entry in
// analysis.DefaultPoolTagMappings, which is the repo owner's own pool. The route
// itself is not hardcoded to this pool - any canonical name from the mapping (or, per
// db.AlgoBucketCountsForPool, any literal unmapped pool_tag) can be passed via ?pool=.
func defaultPoolAlgoBreakdownPool() string {
	if len(analysis.DefaultPoolTagMappings) == 0 {
		return ""
	}
	return analysis.DefaultPoolTagMappings[0].CanonicalName
}

// poolAlgoBreakdownImgSrc builds the `<img src>` value for the pool-algo-breakdown PNG
// endpoint, extending imgSrc's params with the pool name, matching the existing
// analysisParams.String()-based pattern.
func poolAlgoBreakdownImgSrc(pngPath, pool string, p analysisParams) template.URL {
	return template.URL(pngPath + "?" + p.String() + "&pool=" + template.URLQueryEscaper(pool))
}

// parsePoolAlgoBreakdownPool reads ?pool= from the request, defaulting to
// defaultPoolAlgoBreakdownPool() when absent/blank - same graceful-default philosophy
// as parseAnalysisParams (a bad/missing param degrades to a sensible default rather
// than a 400, since this is a read-only reporting page).
func parsePoolAlgoBreakdownPool(r *http.Request) string {
	if pool := r.URL.Query().Get("pool"); pool != "" {
		return pool
	}
	return defaultPoolAlgoBreakdownPool()
}

func (s *Server) handleAnalysisPoolAlgoBreakdown(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	pool := parsePoolAlgoBreakdownPool(r)
	poolOptions, err := analysis.PoolOptions(r.Context(), s.DB, analysis.DefaultPoolTagMappings)
	if err != nil {
		log.Printf("server: analysis pool algo breakdown: pool options: %v", err)
	}
	data := analysisViewData{
		Title:       "Pool Algo Breakdown",
		ImgSrc:      poolAlgoBreakdownImgSrc("/analysis/pool-algo-breakdown.png", pool, p),
		Params:      p,
		PoolField:   true,
		Pool:        pool,
		PoolOptions: poolOptions,
	}
	if pool == "" {
		data.Error = "no pool specified and no default pool tag mapping configured"
	} else if points, order, err := analysis.PoolAlgoBreakdown(r.Context(), s.DB, p.BucketSize, p.From, p.To, analysis.DefaultPoolTagMappings, pool); err != nil {
		// Note: a too-few-buckets result (bucket_size/from/to collapsing the range)
		// is not an error here - analysis.PoolAlgoBreakdown only returns err on a
		// real underlying query failure. The .png endpoint below renders a graceful
		// placeholder for the too-few-buckets shape instead of erroring (see
		// internal/chartrender's doc comments), so this page renders normally
		// (placeholder image and all) for that case without any special-casing here.
		log.Printf("server: analysis pool algo breakdown: %v", err)
		data.Error = "unable to load chart data"
	} else {
		data.Table = newAnalysisTableView(points, order, true)
	}
	if err := s.analysisViewTmpl.Execute(w, data); err != nil {
		log.Printf("server: render analysis pool algo breakdown: %v", err)
	}
}

// writePNG writes chart PNG bytes with the correct Content-Type, or a 500 on error.
func writePNG(w http.ResponseWriter, png []byte, err error, logCtx string) {
	if err != nil {
		log.Printf("server: %s: %v", logCtx, err)
		http.Error(w, "failed to render chart", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	if _, err := w.Write(png); err != nil {
		log.Printf("server: %s: write response: %v", logCtx, err)
	}
}

func (s *Server) handleAnalysisAlgoDistributionPNG(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	points, order, err := analysis.AlgoDistribution(r.Context(), s.DB, p.BucketSize, p.From, p.To)
	if err != nil {
		writePNG(w, nil, err, "analysis algo distribution png")
		return
	}
	png, err := chartrender.StackedAreaChart(points, order, "Algo Distribution", "block height", "block count")
	writePNG(w, png, err, "analysis algo distribution png")
}

func (s *Server) handleAnalysisPoolSharePNG(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	points, order, err := analysis.PoolShare(r.Context(), s.DB, p.BucketSize, p.From, p.To, analysis.DefaultTopPools, analysis.DefaultPoolTagMappings)
	if err != nil {
		writePNG(w, nil, err, "analysis pool share png")
		return
	}
	png, err := chartrender.StackedAreaChart(points, order, "Pool Share", "block height", "block count")
	writePNG(w, png, err, "analysis pool share png")
}

func (s *Server) handleAnalysisBlockTimePNG(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	points, _, err := analysis.BlockTime(r.Context(), s.DB, p.BucketSize, p.From, p.To)
	if err != nil {
		writePNG(w, nil, err, "analysis block time png")
		return
	}
	png, err := chartrender.LineChart(points, []string{"block time (s)"}, "Block Time (median, seconds)", "block height", "seconds")
	writePNG(w, png, err, "analysis block time png")
}

// handleAnalysisDifficultyPNG renders the difficulty chart as 4 separate overlaid lines
// (chartrender.LineChart), one per pow_algo, rather than a chartrender.StackedAreaChart
// like AlgoDistribution/PoolShare/PoolAlgoBreakdown use. Those other views correctly
// stack, because summing block COUNTS across algos gives a real, meaningful total block
// count. Difficulty is not meaningfully additive the same way: RXM/RXT/C29/SHA3X
// difficulties differ by orders of magnitude and measure entirely different,
// non-comparable proof-of-work spaces, so summing them on top of each other would
// produce a number with no real-world interpretation, and would visually crush the
// smaller-magnitude algo's line into an unreadable sliver near the bottom of the stack.
// 4 independent lines, each readable at its own natural scale, is the only correct
// representation here.
func (s *Server) handleAnalysisDifficultyPNG(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	points, order, err := analysis.Difficulty(r.Context(), s.DB, p.BucketSize, p.From, p.To)
	if err != nil {
		writePNG(w, nil, err, "analysis difficulty png")
		return
	}
	png, err := chartrender.LineChart(points, order, "Difficulty (avg per algo, hashrate proxy)", "block height", "difficulty")
	writePNG(w, png, err, "analysis difficulty png")
}

func (s *Server) handleAnalysisPoolAlgoBreakdownPNG(w http.ResponseWriter, r *http.Request) {
	p := s.parseAnalysisParams(r)
	pool := parsePoolAlgoBreakdownPool(r)
	if pool == "" {
		writePNG(w, nil, fmt.Errorf("no pool specified"), "analysis pool algo breakdown png")
		return
	}
	points, order, err := analysis.PoolAlgoBreakdown(r.Context(), s.DB, p.BucketSize, p.From, p.To, analysis.DefaultPoolTagMappings, pool)
	if err != nil {
		writePNG(w, nil, err, "analysis pool algo breakdown png")
		return
	}
	png, err := chartrender.StackedAreaChart(points, order, "Pool Algo Breakdown: "+pool, "block height", "block count")
	writePNG(w, png, err, "analysis pool algo breakdown png")
}
