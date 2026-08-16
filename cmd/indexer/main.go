// Command indexer walks Tari base-node blocks and persists pool-attributed summaries
// into Postgres. Supports both a one-shot backfill and a "follow the tip" mode.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Snipa22/go-tari-explorer/internal/config"
	"github.com/Snipa22/go-tari-explorer/internal/db"
	"github.com/Snipa22/go-tari-explorer/internal/indexer"
	"github.com/Snipa22/go-tari-explorer/internal/nodeclient"
)

func main() {
	postgresDSN := flag.String("postgres-dsn", config.PostgresDSN(), "Postgres connection string (env: TARI_EXPLORER_POSTGRES_DSN)")
	nodeHosts := flag.String("base-node-grpc-hosts", envOrJoin("TARI_EXPLORER_NODE_HOSTS", config.NodeGRPCHosts()), "Comma-separated list of base-node GRPC host:port targets")
	mode := flag.String("mode", "follow", `Indexing mode: "backfill" (one-shot, requires -from/-to) or "follow" (default; keeps polling for new blocks)`)
	from := flag.Uint64("from", 0, "Backfill mode: starting height (inclusive)")
	to := flag.Uint64("to", 0, "Backfill mode: ending height (inclusive). 0 (default) means \"current chain tip\"")
	pollInterval := flag.Duration("poll-interval", 30*time.Second, "Follow mode: how often to check for new blocks")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	hosts := config.ParseHostList(*nodeHosts)
	node, err := nodeclient.New(hosts)
	if err != nil {
		log.Fatalf("indexer: %v", err)
	}
	defer node.Close()

	database, err := db.Connect(ctx, *postgresDSN)
	if err != nil {
		log.Fatalf("indexer: %v", err)
	}
	defer database.Close()

	if err := database.Migrate(ctx); err != nil {
		log.Fatalf("indexer: migrate: %v", err)
	}

	ix := indexer.New(node, database)

	switch *mode {
	case "backfill":
		endHeight := *to
		if endHeight == 0 {
			tip, err := node.GetTipInfo(ctx)
			if err != nil {
				log.Fatalf("indexer: backfill: get tip info: %v", err)
			}
			endHeight = tip.GetMetadata().GetBestBlockHeight()
		}
		log.Printf("indexer: backfilling heights %d-%d", *from, endHeight)
		if err := ix.Backfill(ctx, *from, endHeight); err != nil {
			log.Fatalf("indexer: backfill: %v", err)
		}
		log.Printf("indexer: backfill complete")
	case "follow":
		log.Printf("indexer: following tip (poll interval %s) across hosts %v", *pollInterval, hosts)
		if err := ix.Follow(ctx, *pollInterval); err != nil && ctx.Err() == nil {
			log.Fatalf("indexer: follow: %v", err)
		}
		log.Printf("indexer: shutting down")
	default:
		log.Fatalf("indexer: unknown -mode %q (want \"backfill\" or \"follow\")", *mode)
	}
}

// envOrJoin returns the raw env var if set (so the flag's default reflects it verbatim,
// comma-separated string form), else re-joins the already-parsed default host list -
// this keeps -base-node-grpc-hosts's displayed default useful in `-h` output either way.
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
