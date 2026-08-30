// Command difficulty-poller polls this explorer's own already-indexed `blocks` table
// (via internal/db.DB.CurrentDifficultyPerAlgo - no live base-node GRPC call needed,
// since each of RXM/RXT/C29/SHA3X runs its own independent difficulty-adjustment
// window, so the difficulty on that algo's latest indexed block already is its live
// current target) on a short configurable interval and upserts one difficulty_snapshots
// row per (algo, height) pair actually observed (via internal/difficultypoller.Poller +
// internal/db).
//
// A standalone binary rather than a third cmd/mempool-poller mode or a cmd/indexer
// -mode, for the same reasons cmd/mempool-poller is its own binary (see that command's
// doc comment): different dependency graph (this one needs only *db.DB, no
// nodeclient.Client at all), and this repo's existing precedent of "one small
// standalone binary per additional Postgres-only utility" (cmd/algobuckets,
// cmd/mempool-poller).
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Snipa22/go-tari-explorer/internal/config"
	"github.com/Snipa22/go-tari-explorer/internal/db"
	"github.com/Snipa22/go-tari-explorer/internal/difficultypoller"
)

func main() {
	postgresDSN := flag.String("postgres-dsn", config.PostgresDSN(), "Postgres connection string (env: TARI_EXPLORER_POSTGRES_DSN)")
	pollInterval := flag.Duration("poll-interval", config.DifficultyPollInterval(), "How often to check `blocks` for a per-algo height advance and upsert a new difficulty_snapshots row (env: TARI_EXPLORER_DIFFICULTY_POLL_INTERVAL)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	database, err := db.Connect(ctx, *postgresDSN)
	if err != nil {
		log.Fatalf("difficulty-poller: %v", err)
	}
	defer database.Close()

	if err := database.Migrate(ctx); err != nil {
		log.Fatalf("difficulty-poller: migrate: %v", err)
	}

	poller := difficultypoller.New(database, database)

	log.Printf("difficulty-poller: polling per-algo blocks for height changes every %s", *pollInterval)
	if err := poller.Run(ctx, *pollInterval); err != nil && ctx.Err() == nil {
		log.Fatalf("difficulty-poller: run: %v", err)
	}
	log.Printf("difficulty-poller: shutting down")
}
