// Package server implements a minimal HTTP server (Go's standard net/http +
// html/template, HTMX for the "load more" pagination interaction) rendering a paginated
// blocks list and a single block detail page, reading from the same Postgres blocks
// table the indexer writes to. No JS framework/build step - HTMX is loaded from a CDN
// script tag in the layout template.
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
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Snipa22/go-tari-explorer/internal/db"
	"github.com/Snipa22/go-tari-explorer/internal/poolstats"
)

//go:embed templates/*.html
var templateFS embed.FS

// PageSize is the number of blocks returned per page/HTMX "load more" request.
const PageSize = 25

// funcs is the html/template FuncMap shared by every parsed template.
var funcs = template.FuncMap{
	"sub": func(a, b int) int { return a - b },
}

// Server holds the dependencies needed to serve HTTP requests.
type Server struct {
	DB        *db.DB
	PoolStats poolstats.PoolStatsProvider
	// PoolStatsBaseURL is displayed on the pool-stats page as a "source" attribution -
	// purely informational, not used for any request.
	PoolStatsBaseURL  string
	listTmpl          *template.Template
	detailTmpl        *template.Template
	rowsTmpl          *template.Template
	poolStatsTmpl     *template.Template
	analysisIndexTmpl *template.Template
	analysisViewTmpl  *template.Template
}

// New parses the embedded templates and constructs a Server. Returns an error if the
// templates fail to parse (a build-time programming error, not a runtime/request error).
func New(database *db.DB, poolStatsProvider poolstats.PoolStatsProvider, poolStatsBaseURL string) (*Server, error) {
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
	return &Server{
		DB:                database,
		PoolStats:         poolStatsProvider,
		PoolStatsBaseURL:  poolStatsBaseURL,
		listTmpl:          listTmpl,
		detailTmpl:        detailTmpl,
		rowsTmpl:          rowsTmpl,
		poolStatsTmpl:     poolStatsTmpl,
		analysisIndexTmpl: analysisIndexTmpl,
		analysisViewTmpl:  analysisViewTmpl,
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
	mux.HandleFunc("GET /analysis/block-time", s.handleAnalysisBlockTime)
	mux.HandleFunc("GET /analysis/difficulty", s.handleAnalysisDifficulty)
	mux.HandleFunc("GET /analysis/algo-distribution.png", s.handleAnalysisAlgoDistributionPNG)
	mux.HandleFunc("GET /analysis/pool-share.png", s.handleAnalysisPoolSharePNG)
	mux.HandleFunc("GET /analysis/block-time.png", s.handleAnalysisBlockTimePNG)
	mux.HandleFunc("GET /analysis/difficulty.png", s.handleAnalysisDifficultyPNG)
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
	data := struct {
		Block      blockView
		PrevHeight uint64
	}{Block: blockView{block}, PrevHeight: prevHeight}
	if err := s.detailTmpl.Execute(w, data); err != nil {
		log.Printf("server: render block detail: %v", err)
	}
}
