// Command server runs the go-tari-explorer read-only HTTP UI: a paginated blocks list,
// a per-block detail page (including per-kernel/per-output breakdown), transaction
// search, and a live transaction-state check, rendered server-side from the Postgres
// tables the indexer populates plus live GRPC calls to a configured base node.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/Snipa22/go-tari-explorer/internal/config"
	"github.com/Snipa22/go-tari-explorer/internal/db"
	"github.com/Snipa22/go-tari-explorer/internal/nodeclient"
	"github.com/Snipa22/go-tari-explorer/internal/poolstats"
	"github.com/Snipa22/go-tari-explorer/internal/server"
	"github.com/Snipa22/go-tari-explorer/internal/txsearch"
)

const shutdownTimeout = 10 * time.Second

func main() {
	postgresDSN := flag.String("postgres-dsn", config.PostgresDSN(), "Postgres connection string (env: TARI_EXPLORER_POSTGRES_DSN)")
	httpAddr := flag.String("http-addr", config.HTTPListenAddr(), "HTTP listen address (env: TARI_EXPLORER_HTTP_ADDR)")
	poolStatsBaseURL := flag.String("pool-stats-base-url", config.PoolStatsBaseURL(), "Base URL of the nodejs-pool stats API (env: TARI_EXPLORER_POOL_STATS_BASE_URL)")
	nodeHostsFlag := flag.String("base-node-grpc-hosts", "", "Comma-separated base-node GRPC host:port list for live search/tx-state lookups (env: TARI_EXPLORER_NODE_HOSTS); overrides the env/default if set")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	database, err := db.Connect(ctx, *postgresDSN)
	if err != nil {
		log.Fatalf("server: %v", err)
	}
	defer database.Close()

	if err := database.Migrate(ctx); err != nil {
		log.Fatalf("server: migrate: %v", err)
	}

	nodeHosts := config.NodeGRPCHosts()
	if *nodeHostsFlag != "" {
		nodeHosts = config.ParseHostList(*nodeHostsFlag)
	}
	// A live-search/tx-state Searcher, and the live mempool tx-list/stats /mempool
	// route (+ the front page's condensed mempool-stats panel), all require at least
	// one configured base-node host. This is optional for the server to run at all -
	// those routes/panels degrade to an "unavailable" response (see
	// internal/server.handleSearch/handleTxState/handleMempool/handleBlocksList)
	// rather than the whole process refusing to start, since the blocks list/detail
	// pages don't need it. node is deliberately shared between searcher and
	// server.New's Node param - one dialed *nodeclient.Client, not two - per
	// internal/nodeclient's own single-connection-per-host design.
	var searcher *txsearch.Searcher
	var node *nodeclient.Client
	if len(nodeHosts) > 0 {
		var err error
		node, err = nodeclient.New(nodeHosts)
		if err != nil {
			log.Printf("server: search/mempool unavailable: %v", err)
			node = nil
		} else {
			searcher = txsearch.New(database, node)
		}
	}

	poolStatsClient := poolstats.NewHTTPClient(*poolStatsBaseURL, nil)
	srv, err := server.New(database, poolStatsClient, *poolStatsBaseURL, searcher, node)
	if err != nil {
		log.Fatalf("server: %v", err)
	}

	httpSrv := &http.Server{Addr: *httpAddr, Handler: srv.Handler()}

	go func() {
		<-ctx.Done()
		log.Printf("server: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("server: listening on %s (postgres: %s)", *httpAddr, *postgresDSN)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}
