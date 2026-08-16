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
	DB         *db.DB
	listTmpl   *template.Template
	detailTmpl *template.Template
	rowsTmpl   *template.Template
}

// New parses the embedded templates and constructs a Server. Returns an error if the
// templates fail to parse (a build-time programming error, not a runtime/request error).
func New(database *db.DB) (*Server, error) {
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
	return &Server{DB: database, listTmpl: listTmpl, detailTmpl: detailTmpl, rowsTmpl: rowsTmpl}, nil
}

// Handler builds the top-level http.Handler with all routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleBlocksList)
	mux.HandleFunc("GET /blocks/partial", s.handleBlocksPartial)
	mux.HandleFunc("GET /blocks/{height}", s.handleBlockDetail)
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
