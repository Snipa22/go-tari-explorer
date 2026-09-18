// Command template-difficulty-poller polls a Tari base node's LIVE block-template RPC
// (GetNewBlockTemplate, via internal/nodeclient) on a configurable interval, once per
// pow-algo, and upserts one template_difficulty_snapshots row per (algo, height) pair
// actually observed (via internal/templatepoller.Poller + internal/db).
//
// This is the forward-looking counterpart to cmd/difficulty-poller: that binary reads
// the difficulty already stamped on an algo's most-recently INDEXED block (a read of
// this repo's own already-indexed `blocks` table, no live GRPC call needed); this one
// reads the target difficulty for the NEXT block an algo hasn't mined yet, straight
// off the live daemon - so, unlike cmd/difficulty-poller (DB-only), this binary needs
// both a *nodeclient.Client (to reach the live daemon) AND a *db.DB (to persist
// snapshots), the same two-dependency shape as cmd/mempool-poller.
//
// A standalone binary rather than a third cmd/indexer -mode or folded into
// cmd/difficulty-poller, for the same "one small standalone binary per additional
// Postgres-only/GRPC-polling utility" precedent already established by
// cmd/difficulty-poller and cmd/mempool-poller (see their own doc comments).
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
	"github.com/Snipa22/go-tari-explorer/internal/nodeclient"
	"github.com/Snipa22/go-tari-explorer/internal/templatepoller"
)

func main() {
	postgresDSN := flag.String("postgres-dsn", config.PostgresDSN(), "Postgres connection string (env: TARI_EXPLORER_POSTGRES_DSN)")
	nodeHosts := flag.String("base-node-grpc-hosts", envOrJoin("TARI_EXPLORER_NODE_HOSTS", config.NodeGRPCHosts()), "Comma-separated list of base-node GRPC host:port targets")
	pollInterval := flag.Duration("poll-interval", config.TemplateDifficultyPollInterval(), "How often to poll GetNewBlockTemplate per algo and upsert a new template_difficulty_snapshots row (env: TARI_EXPLORER_TEMPLATE_DIFFICULTY_POLL_INTERVAL)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	hosts := config.ParseHostList(*nodeHosts)
	node, err := nodeclient.New(hosts)
	if err != nil {
		log.Fatalf("template-difficulty-poller: %v", err)
	}
	defer node.Close()

	database, err := db.Connect(ctx, *postgresDSN)
	if err != nil {
		log.Fatalf("template-difficulty-poller: %v", err)
	}
	defer database.Close()

	if err := database.Migrate(ctx); err != nil {
		log.Fatalf("template-difficulty-poller: migrate: %v", err)
	}

	poller := templatepoller.New(node, database)

	log.Printf("template-difficulty-poller: polling GetNewBlockTemplate (per algo) every %s across hosts %v", *pollInterval, hosts)
	if err := poller.Run(ctx, *pollInterval); err != nil && ctx.Err() == nil {
		log.Fatalf("template-difficulty-poller: run: %v", err)
	}
	log.Printf("template-difficulty-poller: shutting down")
}

// envOrJoin mirrors cmd/indexer/main.go's and cmd/mempool-poller/main.go's own helper
// of the same name (see either for the full rationale): returns the raw env var if
// set, else re-joins the already-parsed default host list, so -base-node-grpc-hosts's
// displayed default in `-h` output stays useful either way. Kept as its own small copy
// here rather than a shared exported helper, matching those two binaries' existing
// precedent of not coupling independent binaries over a 10-line, purely cosmetic
// flag-default convenience.
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
