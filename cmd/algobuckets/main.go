// Command algobuckets reports how many blocks were mined on each pow-algo
// (RXM/RXT/C29/SHA3X, see internal/poolattr.PowAlgo) within configurable block-height
// buckets, reading from the same Postgres blocks table the indexer populates. Intended
// to be run once per desired -bucket-size (e.g. once with 1000, again with 10000) and
// the plain-text table output pasted into a report/Discord code block.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Snipa22/go-tari-explorer/internal/config"
	"github.com/Snipa22/go-tari-explorer/internal/db"
)

func main() {
	postgresDSN := flag.String("postgres-dsn", config.PostgresDSN(), "Postgres connection string (env: TARI_EXPLORER_POSTGRES_DSN)")
	bucketSize := flag.Uint64("bucket-size", 1000, "Number of consecutive block heights per bucket")
	from := flag.Uint64("from", 0, "Starting height (inclusive)")
	to := flag.Uint64("to", 0, "Ending height (inclusive). 0 (default) means \"max indexed height\"")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *bucketSize == 0 {
		log.Fatalf("algobuckets: -bucket-size must be > 0")
	}

	database, err := db.Connect(ctx, *postgresDSN)
	if err != nil {
		log.Fatalf("algobuckets: %v", err)
	}
	defer database.Close()

	toHeight := *to
	if toHeight == 0 {
		toHeight, err = database.MaxIndexedHeight(ctx)
		if err != nil {
			log.Fatalf("algobuckets: max indexed height: %v", err)
		}
	}

	rows, err := database.AlgoBucketCounts(ctx, *bucketSize, *from, toHeight)
	if err != nil {
		log.Fatalf("algobuckets: %v", err)
	}

	printTable(os.Stdout, rows)
}

// printTable renders rows as an aligned, pipe-delimited plain-text table matching the
// "block_start | block_end | RXM | RXT | C29 | SHA3X" report shape.
func printTable(w *os.File, rows []db.AlgoBucketRow) {
	fmt.Fprintf(w, "%-12s| %-10s| %-6s| %-6s| %-6s| %-6s\n", "block_start", "block_end", "RXM", "RXT", "C29", "SHA3X")
	for _, r := range rows {
		fmt.Fprintf(w, "%-12d| %-10d| %-6d| %-6d| %-6d| %-6d\n", r.BucketStart, r.BucketEnd, r.RXM, r.RXT, r.C29, r.SHA3X)
	}
}
