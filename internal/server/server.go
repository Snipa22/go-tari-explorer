// Package server implements a minimal HTTP server (Go's standard net/http +
// html/template, HTMX for the "load more" pagination interaction) rendering a paginated
// blocks list, a single block detail page (including its per-kernel/per-output
// breakdown), a transaction search page, and a live transaction-state check, reading
// from the same Postgres tables the indexer writes to (plus, for search/tx-state, live
// GRPC calls to the base node via internal/txsearch). No JS framework/build step - HTMX
// is loaded from a CDN script tag in the layout template.
package server

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/Snipa22/go-tari-explorer/internal/db"
	"github.com/Snipa22/go-tari-explorer/internal/poolstats"
	"github.com/Snipa22/go-tari-explorer/internal/txsearch"
)

//go:embed templates/*.html
var templateFS embed.FS

// PageSize is the number of blocks returned per page/HTMX "load more" request.
const PageSize = 25

// microMinotariPerXTM is the conversion factor for displaying kernel fees (stored as
// raw MicroMinotari, matching the wire type) as human-readable XTM.
const microMinotariPerXTM = 1_000_000

// funcs is the html/template FuncMap shared by every parsed template.
var funcs = template.FuncMap{
	"sub": func(a, b int) int { return a - b },
}

// Server holds the dependencies needed to serve HTTP requests.
type Server struct {
	DB        *db.DB
	PoolStats poolstats.PoolStatsProvider
	Search    *txsearch.Searcher // nil-safe: a nil Searcher makes /search and /tx-state report "not configured" rather than panicking
	// PoolStatsBaseURL is displayed on the pool-stats page as a "source" attribution -
	// purely informational, not used for any request.
	PoolStatsBaseURL  string
	listTmpl          *template.Template
	detailTmpl        *template.Template
	rowsTmpl          *template.Template
	poolStatsTmpl     *template.Template
	analysisIndexTmpl *template.Template
	analysisViewTmpl  *template.Template
	searchTmpl        *template.Template
	txStateTmpl       *template.Template
}

// New parses the embedded templates and constructs a Server. Returns an error if the
// templates fail to parse (a build-time programming error, not a runtime/request error).
// searcher may be nil (e.g. no base-node hosts configured for search) - /search and
// /tx-state still render, reporting search as unavailable rather than 500ing.
func New(database *db.DB, poolStatsProvider poolstats.PoolStatsProvider, poolStatsBaseURL string, searcher *txsearch.Searcher) (*Server, error) {
	listTmpl, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/blocks_list.html")
	if err != nil {
		return nil, fmt.Errorf("server: parse blocks list templates: %w", err)
	}
	detailTmpl, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/block_detail.html")
	if err != nil {
		return nil, fmt.Errorf("server: parse block detail templates: %w", err)
	}
	rowsTmpl, err := template.New("rows-only").Funcs(funcs).ParseFS(templateFS, "templates/blocks_list.html")
	if err != nil {
		return nil, fmt.Errorf("server: parse rows template: %w", err)
	}
	poolStatsTmpl, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/pool_stats.html")
	if err != nil {
		return nil, fmt.Errorf("server: parse pool stats template: %w", err)
	}
	analysisIndexTmpl, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/analysis.html")
	if err != nil {
		return nil, fmt.Errorf("server: parse analysis index template: %w", err)
	}
	analysisViewTmpl, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/analysis_view.html")
	if err != nil {
		return nil, fmt.Errorf("server: parse analysis view template: %w", err)
	}
	searchTmpl, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/search_result.html")
	if err != nil {
		return nil, fmt.Errorf("server: parse search result template: %w", err)
	}
	txStateTmpl, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/tx_state.html")
	if err != nil {
		return nil, fmt.Errorf("server: parse tx state template: %w", err)
	}
	return &Server{
		DB:                database,
		PoolStats:         poolStatsProvider,
		Search:            searcher,
		PoolStatsBaseURL:  poolStatsBaseURL,
		listTmpl:          listTmpl,
		detailTmpl:        detailTmpl,
		rowsTmpl:          rowsTmpl,
		poolStatsTmpl:     poolStatsTmpl,
		analysisIndexTmpl: analysisIndexTmpl,
		analysisViewTmpl:  analysisViewTmpl,
		searchTmpl:        searchTmpl,
		txStateTmpl:       txStateTmpl,
	}, nil
}

// Handler builds the top-level http.Handler with all routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleBlocksList)
	mux.HandleFunc("GET /blocks/partial", s.handleBlocksPartial)
	mux.HandleFunc("GET /blocks/{height}", s.handleBlockDetail)
	mux.HandleFunc("GET /pool-stats", s.handlePoolStats)
	mux.HandleFunc("GET /analysis", s.handleAnalysisIndex)
	mux.HandleFunc("GET /analysis/algo-distribution", s.handleAnalysisAlgoDistribution)
	mux.HandleFunc("GET /analysis/pool-share", s.handleAnalysisPoolShare)
	mux.HandleFunc("GET /analysis/pool-algo-breakdown", s.handleAnalysisPoolAlgoBreakdown)
	mux.HandleFunc("GET /analysis/block-time", s.handleAnalysisBlockTime)
	mux.HandleFunc("GET /analysis/difficulty", s.handleAnalysisDifficulty)
	mux.HandleFunc("GET /analysis/algo-distribution.png", s.handleAnalysisAlgoDistributionPNG)
	mux.HandleFunc("GET /analysis/pool-share.png", s.handleAnalysisPoolSharePNG)
	mux.HandleFunc("GET /analysis/pool-algo-breakdown.png", s.handleAnalysisPoolAlgoBreakdownPNG)
	mux.HandleFunc("GET /analysis/block-time.png", s.handleAnalysisBlockTimePNG)
	mux.HandleFunc("GET /analysis/difficulty.png", s.handleAnalysisDifficultyPNG)
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("GET /tx-state", s.handleTxState)
	return mux
}

// blockView adapts db.Block for template rendering (human-friendly timestamp, pool
// display string/CSS class) without leaking presentation concerns into the db package.
type blockView struct {
	db.Block
}

func (b blockView) TimeString() string {
	return time.Unix(b.Timestamp, 0).UTC().Format("2006-01-02 15:04:05 UTC")
}

func (b blockView) PoolDisplay() string {
	if b.PoolTag == nil || *b.PoolTag == "" {
		return "unknown"
	}
	return *b.PoolTag
}

func (b blockView) PoolCSSClass() string {
	if b.PoolTag == nil || *b.PoolTag == "" {
		return "pool-unknown"
	}
	return "pool-own"
}

func toBlockViews(blocks []db.Block) []blockView {
	out := make([]blockView, len(blocks))
	for i, b := range blocks {
		out[i] = blockView{b}
	}
	return out
}

func (s *Server) handleBlocksList(w http.ResponseWriter, r *http.Request) {
	blocks, err := s.DB.ListBlocks(r.Context(), math.MaxInt64, PageSize)
	if err != nil {
		http.Error(w, "failed to load blocks", http.StatusInternalServerError)
		log.Printf("server: list blocks: %v", err)
		return
	}
	data := struct{ Blocks []blockView }{Blocks: toBlockViews(blocks)}
	if err := s.listTmpl.Execute(w, data); err != nil {
		log.Printf("server: render blocks list: %v", err)
	}
}

// handleBlocksPartial serves the HTMX "load more" request: the next page of rows
// strictly below ?before=<height>, rendered without the surrounding page layout.
func (s *Server) handleBlocksPartial(w http.ResponseWriter, r *http.Request) {
	before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if err != nil {
		http.Error(w, "invalid before parameter", http.StatusBadRequest)
		return
	}
	blocks, err := s.DB.ListBlocks(r.Context(), before, PageSize)
	if err != nil {
		http.Error(w, "failed to load blocks", http.StatusInternalServerError)
		log.Printf("server: list blocks partial: %v", err)
		return
	}
	if err := s.rowsTmpl.ExecuteTemplate(w, "rows", toBlockViews(blocks)); err != nil {
		log.Printf("server: render blocks partial: %v", err)
	}
}

// poolStatsView adapts poolstats.PoolStats for template rendering (human-friendly
// hash-rate/timestamp formatting), keeping presentation concerns out of the poolstats
// package itself.
type poolStatsView struct {
	poolstats.PoolStats
}

// HashRateDisplay formats HashRate with a H/s/KH/s/MH/s/GH/s suffix.
func (v poolStatsView) HashRateDisplay() string {
	return formatHashRate(v.HashRate)
}

func formatHashRate(hashRate int64) string {
	const unit = 1000.0
	rate := float64(hashRate)
	if rate < unit {
		return fmt.Sprintf("%d H/s", hashRate)
	}
	units := []string{"KH/s", "MH/s", "GH/s", "TH/s"}
	div, exp := unit, 0
	for r := rate / unit; r >= unit && exp < len(units)-1; r /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %s", rate/div, units[exp])
}

// LastBlockFoundTimeDisplay formats LastBlockFoundTime as a human-readable UTC
// timestamp, or "never" if unset.
func (v poolStatsView) LastBlockFoundTimeDisplay() string {
	if v.LastBlockFoundTime == 0 {
		return "never"
	}
	return time.Unix(v.LastBlockFoundTime, 0).UTC().Format("2006-01-02 15:04:05 UTC")
}

// LastPaymentDisplay formats LastPayment as a human-readable UTC timestamp, or "never"
// if unset.
func (v poolStatsView) LastPaymentDisplay() string {
	if v.LastPayment == 0 {
		return "never"
	}
	return time.Unix(v.LastPayment, 0).UTC().Format("2006-01-02 15:04:05 UTC")
}

// handlePoolStats renders the pool-wide stats page via s.PoolStats (a
// poolstats.PoolStatsProvider). If PoolStats is unconfigured (nil), or the fetch
// fails, the page still renders with an inline error message rather than a 500 - a
// pool-backend outage shouldn't be treated as a server error.
func (s *Server) handlePoolStats(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Stats   poolStatsView
		Error   string
		BaseURL string
	}{BaseURL: s.PoolStatsBaseURL}

	if s.PoolStats == nil {
		data.Error = "pool stats provider not configured"
	} else {
		stats, err := s.PoolStats.GetStats(r.Context())
		if err != nil {
			log.Printf("server: get pool stats: %v", err)
			data.Error = "unable to reach pool stats backend"
		} else {
			data.Stats = poolStatsView{stats}
		}
	}

	if err := s.poolStatsTmpl.Execute(w, data); err != nil {
		log.Printf("server: render pool stats: %v", err)
	}
}

func (s *Server) handleBlockDetail(w http.ResponseWriter, r *http.Request) {
	heightStr := r.PathValue("height")
	height, err := strconv.ParseUint(heightStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid block height", http.StatusBadRequest)
		return
	}
	block, err := s.DB.GetBlock(r.Context(), height)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "failed to load block", http.StatusInternalServerError)
		log.Printf("server: get block %d: %v", height, err)
		return
	}
	var prevHeight uint64
	if block.Height > 0 {
		prevHeight = block.Height - 1
	}

	kernels, err := s.DB.GetKernelsForBlock(r.Context(), height)
	if err != nil {
		http.Error(w, "failed to load kernels", http.StatusInternalServerError)
		log.Printf("server: get kernels for block %d: %v", height, err)
		return
	}
	outputs, err := s.DB.GetOutputsForBlock(r.Context(), height)
	if err != nil {
		http.Error(w, "failed to load outputs", http.StatusInternalServerError)
		log.Printf("server: get outputs for block %d: %v", height, err)
		return
	}

	data := struct {
		Block      blockView
		PrevHeight uint64
		Kernels    []kernelView
		Outputs    []outputView
	}{
		Block:      blockView{block},
		PrevHeight: prevHeight,
		Kernels:    toKernelViews(kernels),
		Outputs:    toOutputViews(outputs),
	}
	if err := s.detailTmpl.Execute(w, data); err != nil {
		log.Printf("server: render block detail: %v", err)
	}
}

// kernelView adapts db.Kernel for template rendering (human-readable fee, truncated
// hex for the excess signature).
type kernelView struct {
	db.Kernel
}

// FeeXTM renders Fee (stored raw, in MicroMinotari) as XTM with up to 6 decimal places.
func (k kernelView) FeeXTM() string {
	return formatMicroMinotari(k.Fee)
}

// ExcessSigHex renders the full public_nonce+signature pair as a single hex string -
// the same 128-char shape /search and /tx-state expect, so this can double as a
// ready-to-copy/search value.
func (k kernelView) ExcessSigHex() string {
	return fmt.Sprintf("%x%x", k.ExcessSigNonce, k.ExcessSigSignature)
}

// ExcessSigHexShort renders a truncated (head...tail) version of ExcessSigHex for
// compact table display.
func (k kernelView) ExcessSigHexShort() string {
	return truncateHex(k.ExcessSigHex())
}

func toKernelViews(kernels []db.Kernel) []kernelView {
	out := make([]kernelView, len(kernels))
	for i, k := range kernels {
		out[i] = kernelView{k}
	}
	return out
}

// outputType names, matching tari_generated.OutputType's four known values by their
// raw wire id. Kept as a small local table (rather than depending on tari_generated
// just for this) since server already receives OutputType as a plain uint32 off
// db.Output.
var outputTypeNames = map[uint32]string{
	0: "STANDARD",
	1: "COINBASE",
	2: "BURN",
	3: "VALIDATOR_NODE_REGISTRATION",
	4: "CODE_TEMPLATE_REGISTRATION",
}

// outputView adapts db.Output for template rendering (output type as a human string,
// sanitized coinbase extra, truncated commitment hex).
type outputView struct {
	db.Output
}

// OutputTypeDisplay renders OutputType as its human name, or the raw number if unknown.
func (o outputView) OutputTypeDisplay() string {
	if name, ok := outputTypeNames[o.OutputType]; ok {
		return name
	}
	return fmt.Sprintf("UNKNOWN(%d)", o.OutputType)
}

// CoinbaseExtraDisplay renders CoinbaseExtra as printable-sanitized text, or "-" when
// empty (most non-coinbase outputs). This is read-only display - the actual
// pool-tagging logic that also reads this field lives in internal/poolattr and isn't
// duplicated here.
func (o outputView) CoinbaseExtraDisplay() string {
	if len(o.CoinbaseExtra) == 0 {
		return "-"
	}
	return sanitizePrintable(o.CoinbaseExtra)
}

// CommitmentHex renders the full commitment hex - the same value /search expects, so
// this can double as a ready-to-copy/search value.
func (o outputView) CommitmentHex() string {
	return fmt.Sprintf("%x", o.Commitment)
}

// CommitmentHexShort renders a truncated (head...tail) version of CommitmentHex for
// compact table display.
func (o outputView) CommitmentHexShort() string {
	return truncateHex(o.CommitmentHex())
}

func toOutputViews(outputs []db.Output) []outputView {
	out := make([]outputView, len(outputs))
	for i, o := range outputs {
		out[i] = outputView{o}
	}
	return out
}

// formatMicroMinotari renders a raw MicroMinotari amount as XTM, trimming trailing
// zeros after the decimal point (but always leaving at least "X.0").
func formatMicroMinotari(microMinotari uint64) string {
	s := strconv.FormatFloat(float64(microMinotari)/float64(microMinotariPerXTM), 'f', 6, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s + " XTM"
}

// truncateHex shortens a hex string to a "head...tail" form for compact table display,
// left untouched if it's already short enough to not need it.
func truncateHex(hexStr string) string {
	const headLen, tailLen = 10, 8
	if len(hexStr) <= headLen+tailLen+3 {
		return hexStr
	}
	return hexStr[:headLen] + "..." + hexStr[len(hexStr)-tailLen:]
}

// sanitizePrintable strips non-printable runes for safe, readable display - the same
// filtering rule internal/poolattr.printableOnly uses for its own (unexported, tagging
// -purpose) rendering, reimplemented here as a small display-only helper so this
// package doesn't need to import poolattr just for a byte filter.
func sanitizePrintable(b []byte) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, string(b))
}

// handleSearch serves GET /search?q=<value>: resolves q via internal/txsearch (indexed
// DB lookup first, live GRPC fallback second - see that package's doc comment) and
// renders either a "found" result linking to the matching block, or a clear
// "not found" state. A missing/blank q renders the bare search form.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	data := struct {
		Query       string
		Searched    bool
		Unavailable bool
		Result      txsearch.Result
		Error       string
	}{Query: query}

	if query != "" {
		data.Searched = true
		if s.Search == nil {
			data.Unavailable = true
		} else {
			result, err := s.Search.Search(r.Context(), query)
			if err != nil {
				log.Printf("server: search %q: %v", query, err)
				data.Error = "search failed - see server logs"
			} else {
				data.Result = result
			}
		}
	}

	if err := s.searchTmpl.Execute(w, data); err != nil {
		log.Printf("server: render search: %v", err)
	}
}

// handleTxState serves GET /tx-state?excess_sig=<hex>: a live (never DB-backed) check
// of a kernel's mempool/mined/unknown status via the base node's TransactionState RPC.
// excess_sig must be the full 128-hex-char (public_nonce+signature) form - see
// internal/txsearch's doc comment on why that's the only unambiguous shape for this
// RPC.
func (s *Server) handleTxState(w http.ResponseWriter, r *http.Request) {
	excessSig := strings.TrimSpace(r.URL.Query().Get("excess_sig"))

	data := struct {
		ExcessSig   string
		Checked     bool
		Unavailable bool
		Result      txsearch.TransactionStateResult
		Error       string
	}{ExcessSig: excessSig}

	if excessSig != "" {
		data.Checked = true
		if s.Search == nil {
			data.Unavailable = true
		} else {
			result, err := s.Search.CheckTransactionState(r.Context(), excessSig)
			if err != nil {
				log.Printf("server: tx-state %q: %v", excessSig, err)
				data.Error = err.Error()
			} else {
				data.Result = result
			}
		}
	}

	if err := s.txStateTmpl.Execute(w, data); err != nil {
		log.Printf("server: render tx-state: %v", err)
	}
}
