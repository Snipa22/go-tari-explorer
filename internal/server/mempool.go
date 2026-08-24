// Live mempool HTTP routes: GET /mempool (the current pending-transaction list plus
// aggregate stats, fetched fresh per request from the configured base node - see
// internal/nodeclient.Client.GetMempoolTransactions/GetMempoolStats) and GET
// /mempool/history plus its companion GET /mempool/history.png (a server-rendered PNG
// chart of unconfirmed_txs/unconfirmed_weight over time, sourced from the
// mempool_snapshots table internal/mempoolpoller populates - see internal/db's
// ListMempoolSnapshots). The history page/PNG pair follows the exact same
// data-query-handler + companion-PNG-render-handler split internal/server/analysis.go
// already uses (e.g. handleAnalysisDifficulty + handleAnalysisDifficultyPNG), just
// without that package's height-bucketing: mempool_snapshots is already one row per
// poll tick, so there's nothing to bucket.
package server

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"

	"github.com/Snipa22/go-tari-explorer/internal/chartrender"
	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// mempoolStatsView adapts a live tari_generated.MempoolStatsResponse for template
// rendering - shared by /mempool's full stat-grid (handleMempool) and the front
// page's condensed summary (handleBlocksList in server.go), since both show exactly
// the same three aggregate numbers.
type mempoolStatsView struct {
	UnconfirmedTxs    uint64
	ReorgTxs          uint64
	UnconfirmedWeight uint64
}

func newMempoolStatsView(stats *tari_generated.MempoolStatsResponse) mempoolStatsView {
	return mempoolStatsView{
		UnconfirmedTxs:    stats.GetUnconfirmedTxs(),
		ReorgTxs:          stats.GetReorgTxs(),
		UnconfirmedWeight: stats.GetUnconfirmedWeight(),
	}
}

// mempoolTxView adapts one live tari_generated.Transaction (as returned by
// GetMempoolTransactions) for template rendering: a truncated excess-sig hex (from
// its first kernel, matching how a transaction's kernel excess-sig is treated as its
// identifier everywhere else in this server - see kernelView.ExcessSigHex), the total
// fee summed across *all* of its kernels (converted MicroMinotari -> XTM via the same
// formatMicroMinotari helper block_detail.html's kernel table uses), and raw
// input/output/kernel counts.
type mempoolTxView struct {
	ExcessSigHex   string
	ExcessSigShort string
	FeeXTM         string
	InputCount     int
	OutputCount    int
	KernelCount    int
}

// toMempoolTxViews adapts a GetMempoolTransactions result for mempool.html's pending-
// transactions table.
func toMempoolTxViews(txs []*tari_generated.Transaction) []mempoolTxView {
	out := make([]mempoolTxView, len(txs))
	for i, tx := range txs {
		body := tx.GetBody()
		kernels := body.GetKernels()

		var totalFee uint64
		var excessSig string
		for k, kernel := range kernels {
			totalFee += kernel.GetFee()
			if k == 0 {
				if sig := kernel.GetExcessSig(); sig != nil {
					excessSig = fmt.Sprintf("%x%x", sig.GetPublicNonce(), sig.GetSignature())
				}
			}
		}

		out[i] = mempoolTxView{
			ExcessSigHex:   excessSig,
			ExcessSigShort: truncateHex(excessSig),
			FeeXTM:         formatMicroMinotari(totalFee),
			InputCount:     len(body.GetInputs()),
			OutputCount:    len(body.GetOutputs()),
			KernelCount:    len(kernels),
		}
	}
	return out
}

// mempoolPageData is the template data shape for mempool.html. The tx-list and stats
// sections are independent: TxError/StatsError are set (with the corresponding data
// field left zero) whenever that section's own live GRPC call failed, so one section
// erroring never blanks out the other - see handleMempool's doc comment for why (some
// real base nodes expose GetMempoolStats but reject GetMempoolTransactions with
// PermissionDenied, or vice versa). TxUnavailable/StatsUnavailable are set instead of
// *Error specifically when s.Node is nil (no base-node host configured at all),
// matching /search and /tx-state's existing Unavailable/Error distinction in
// server.go (handleSearch/handleTxState).
type mempoolPageData struct {
	TxUnavailable bool
	TxCount       int
	Txs           []mempoolTxView
	TxError       string

	StatsUnavailable bool
	Stats            *mempoolStatsView
	StatsError       string
}

// handleMempool serves GET /mempool: the live pending-transaction list
// (GetMempoolTransactions) and live aggregate stats (GetMempoolStats), both fetched
// fresh per request - see internal/nodeclient.Client's doc comments on why neither is
// ever answered from the Postgres index (mempool contents are inherently transient).
//
// s.Node == nil (no base-node GRPC host configured) degrades both sections to an
// "unavailable" message rather than a 500, matching /search and /tx-state's existing
// nil-Search convention. When s.Node is configured, the two live calls are still
// independent of each other: GetMempoolTransactions is verified (against the real
// node-pool.tari.jagtech.io:18102) to return PermissionDenied on at least one real
// base node while GetMempoolStats works fine there, so a failure in either call only
// blanks its own section (with an inline error) rather than failing the whole page.
func (s *Server) handleMempool(w http.ResponseWriter, r *http.Request) {
	data := mempoolPageData{}

	if s.Node == nil {
		data.TxUnavailable = true
		data.StatsUnavailable = true
	} else {
		txs, err := s.Node.GetMempoolTransactions(r.Context())
		if err != nil {
			log.Printf("server: mempool transactions: %v", err)
			data.TxError = "unable to load pending transactions from the base node"
		} else {
			data.TxCount = len(txs)
			data.Txs = toMempoolTxViews(txs)
		}

		stats, err := s.Node.GetMempoolStats(r.Context())
		if err != nil {
			log.Printf("server: mempool stats: %v", err)
			data.StatsError = "unable to load mempool stats from the base node"
		} else {
			v := newMempoolStatsView(stats)
			data.Stats = &v
		}
	}

	if err := s.mempoolTmpl.Execute(w, data); err != nil {
		log.Printf("server: render mempool: %v", err)
	}
}

// mempoolHistoryParams is the parsed/defaulted from/to query-param pair shared by
// handleMempoolHistory and its companion handleMempoolHistoryPNG, matching
// analysisParams' role for the /analysis/* pages. Both are optional - nil means
// unbounded on that side, passed straight through to db.ListMempoolSnapshots.
//
// Values are parsed as RFC3339 timestamps (e.g. "2026-08-24T00:00:00Z"), chosen over
// raw Unix timestamps for human-readability in a hand-edited/bookmarked query string -
// this is a read-only reporting page in the same spirit as /analysis/*, so a
// copy-pasteable, human-readable param was preferred over a marginally terser one. A
// malformed value is silently treated as absent (falls back to unbounded on that
// side) rather than a 400, matching parseAnalysisParams' "bad param degrades
// gracefully" philosophy for this same class of read-only reporting page.
type mempoolHistoryParams struct {
	From *time.Time
	To   *time.Time
}

// String renders the params back into a query string, used to build the HTML page's
// <img src="...png?..."> consistently - same rationale as analysisParams.String().
func (p mempoolHistoryParams) String() string {
	var parts []string
	if p.From != nil {
		parts = append(parts, "from="+p.From.UTC().Format(time.RFC3339))
	}
	if p.To != nil {
		parts = append(parts, "to="+p.To.UTC().Format(time.RFC3339))
	}
	return strings.Join(parts, "&")
}

// parseMempoolHistoryParams reads from/to from the request's query string per
// mempoolHistoryParams' doc comment.
func parseMempoolHistoryParams(r *http.Request) mempoolHistoryParams {
	q := r.URL.Query()
	var p mempoolHistoryParams
	if v := q.Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			p.From = &t
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			p.To = &t
		}
	}
	return p
}

// mempoolHistoryImgSrc builds the `<img src>` value for /mempool/history.png given the
// current request's params, as a template.URL for the same reason imgSrc (in
// analysis.go) is - so html/template's contextual autoescaper leaves the query-string
// separators (&, =) literal.
func mempoolHistoryImgSrc(p mempoolHistoryParams) template.URL {
	if qs := p.String(); qs != "" {
		return template.URL("/mempool/history.png?" + qs)
	}
	return template.URL("/mempool/history.png")
}

// handleMempoolHistory serves GET /mempool/history: the HTML page embedding the
// unconfirmed_txs/unconfirmed_weight-over-time PNG chart (see handleMempoolHistoryPNG)
// as an <img> tag, following the exact same page/PNG split as e.g.
// handleAnalysisDifficulty/handleAnalysisDifficultyPNG.
func (s *Server) handleMempoolHistory(w http.ResponseWriter, r *http.Request) {
	p := parseMempoolHistoryParams(r)
	data := struct {
		ImgSrc template.URL
	}{ImgSrc: mempoolHistoryImgSrc(p)}
	if err := s.mempoolHistoryTmpl.Execute(w, data); err != nil {
		log.Printf("server: render mempool history: %v", err)
	}
}

// mempoolHistorySeriesOrder is the fixed series order/legend for the mempool-history
// line chart: pending transaction count and total unconfirmed weight, plotted
// together against the same time axis (see handleMempoolHistoryPNG). The two series
// share a y-axis despite having very different natural scales (a transaction count in
// the tens/hundreds vs. a byte-ish weight figure that can run much higher) - the same
// simplification internal/chartrender.LineChart's existing multi-series support
// already implies for any multi-series caller; a dual-y-axis chart would be a bigger
// feature than this task calls for.
var mempoolHistorySeriesOrder = []string{"unconfirmed txs", "unconfirmed weight"}

// mempoolSnapshotPoints adapts db.ListMempoolSnapshots' result into
// chartrender.Points for mempoolHistorySeriesOrder, one point per snapshot row, X
// being the snapshot's Unix timestamp (seconds).
func mempoolSnapshotPoints(snapshots []db.MempoolSnapshot) []chartrender.Point {
	points := make([]chartrender.Point, len(snapshots))
	for i, snap := range snapshots {
		points[i] = chartrender.Point{
			X: float64(snap.SnapshotTime.Unix()),
			Series: map[string]float64{
				"unconfirmed txs":    float64(snap.UnconfirmedTxs),
				"unconfirmed weight": float64(snap.UnconfirmedWeight),
			},
		}
	}
	return points
}

// mempoolHistoryMinSnapshotsForChart is the minimum number of mempool_snapshots rows
// needed to draw a meaningful line chart. A single point has no line to draw at all,
// and internal/chartrender.LineChart's own "no points" guard only rejects zero, not
// one - so this handler applies its own slightly stricter floor and falls back to
// chartrender.PlaceholderPNG below it, rather than rendering a degenerate one-point
// "chart" that's more confusing than informative.
const mempoolHistoryMinSnapshotsForChart = 2

// handleMempoolHistoryPNG serves GET /mempool/history.png: the PNG chart companion to
// handleMempoolHistory, sourcing mempool_snapshots rows (see internal/mempoolpoller,
// which populates that table on a poll interval) in the optional [from, to] range and
// rendering them as a two-series line chart. Never 500s on a data-availability
// problem: too few snapshots to plot (0 or 1) renders chartrender.PlaceholderPNG
// instead of erroring - only an actual Postgres query failure is a 500.
func (s *Server) handleMempoolHistoryPNG(w http.ResponseWriter, r *http.Request) {
	p := parseMempoolHistoryParams(r)
	snapshots, err := s.DB.ListMempoolSnapshots(r.Context(), p.From, p.To)
	if err != nil {
		log.Printf("server: mempool history png: list snapshots: %v", err)
		http.Error(w, "failed to load mempool history", http.StatusInternalServerError)
		return
	}

	if len(snapshots) < mempoolHistoryMinSnapshotsForChart {
		png, err := chartrender.PlaceholderPNG()
		writePNG(w, png, err, "mempool history png (insufficient data)")
		return
	}

	points := mempoolSnapshotPoints(snapshots)
	png, err := chartrender.LineChart(points, mempoolHistorySeriesOrder, "Mempool History", "time (unix seconds)", "count / weight")
	writePNG(w, png, err, "mempool history png")
}
