// Command mempool-poller polls a Tari base node's live mempool aggregate stats
// (GetMempoolStats, via internal/nodeclient) on a configurable interval and persists
// exactly one mempool_snapshots row per tick into Postgres (via
// internal/mempoolpoller.Poller + internal/db).
//
// This is a new standalone binary rather than a third cmd/indexer -mode: cmd/indexer's
// existing "backfill"/"follow" modes are both built around the Indexer type (block
// walking + pool attribution + per-block kernel/output upserts) - an entirely
// different dependency graph from this poller's, which is just one unary
// GetMempoolStats call plus one aggregate insert per tick, wired through its own
// injectable-fetcher seam for unit testing (see internal/mempoolpoller.Poller).
// Bolting a third mode onto cmd/indexer's main() would mean threading two unrelated
// sets of flags/dependencies through a single -mode switch for no shared benefit; this
// repo already has precedent for "one small standalone binary per additional
// Postgres-only utility" in cmd/algobuckets, and mempool-poller follows that same
// shape instead.
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
	"github.com/Snipa22/go-tari-explorer/internal/mempoolpoller"
	"github.com/Snipa22/go-tari-explorer/internal/nodeclient"
)

func main() {
	postgresDSN := flag.String("postgres-dsn", config.PostgresDSN(), "Postgres connection string (env: TARI_EXPLORER_POSTGRES_DSN)")
	nodeHosts := flag.String("base-node-grpc-hosts", envOrJoin("TARI_EXPLORER_NODE_HOSTS", config.NodeGRPCHosts()), "Comma-separated list of base-node GRPC host:port targets")
	pollInterval := flag.Duration("poll-interval", config.MempoolPollInterval(), "How often to poll GetMempoolStats and insert a new mempool_snapshots row (env: TARI_EXPLORER_MEMPOOL_POLL_INTERVAL)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	hosts := config.ParseHostList(*nodeHosts)
	node, err := nodeclient.New(hosts)
	if err != nil {
		log.Fatalf("mempool-poller: %v", err)
	}
	defer node.Close()

	database, err := db.Connect(ctx, *postgresDSN)
	if err != nil {
		log.Fatalf("mempool-poller: %v", err)
	}
	defer database.Close()

	if err := database.Migrate(ctx); err != nil {
		log.Fatalf("mempool-poller: migrate: %v", err)
	}

	poller := mempoolpoller.New(node, database)

	log.Printf("mempool-poller: polling GetMempoolStats every %s across hosts %v", *pollInterval, hosts)
	if err := poller.Run(ctx, *pollInterval); err != nil && ctx.Err() == nil {
		log.Fatalf("mempool-poller: run: %v", err)
	}
	log.Printf("mempool-poller: shutting down")
}

// envOrJoin mirrors cmd/indexer/main.go's own helper of the same name (see that file
// for the full rationale): returns the raw env var if set, else re-joins the
// already-parsed default host list, so -base-node-grpc-hosts's displayed default in
// `-h` output stays useful either way. Kept as its own small copy here rather than a
// shared exported helper - a 10-line, purely cosmetic flag-default convenience isn't
// worth coupling two independent binaries over.
func envOrJoin(envVar string, defaultHosts []string) string {
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	joined := ""
	for i, h := range defaultHosts {
		if i > 0 {
			joined += ","
		}
		joined += h
	}
	return joined
}
