// Command server runs the go-tari-explorer read-only HTTP UI: a paginated blocks list
// and per-block detail page, rendered server-side from the Postgres blocks table the
// indexer populates.
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
	"github.com/Snipa22/go-tari-explorer/internal/poolstats"
	"github.com/Snipa22/go-tari-explorer/internal/server"
)

const shutdownTimeout = 10 * time.Second

func main() {
	postgresDSN := flag.String("postgres-dsn", config.PostgresDSN(), "Postgres connection string (env: TARI_EXPLORER_POSTGRES_DSN)")
	httpAddr := flag.String("http-addr", config.HTTPListenAddr(), "HTTP listen address (env: TARI_EXPLORER_HTTP_ADDR)")
	poolStatsBaseURL := flag.String("pool-stats-base-url", config.PoolStatsBaseURL(), "Base URL of the nodejs-pool stats API (env: TARI_EXPLORER_POOL_STATS_BASE_URL)")
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

	poolStatsClient := poolstats.NewHTTPClient(*poolStatsBaseURL, nil)
	srv, err := server.New(database, poolStatsClient, *poolStatsBaseURL)
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
