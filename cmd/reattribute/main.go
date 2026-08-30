// Command reattribute recomputes each already-indexed block's pool_tag by re-running
// internal/poolattr.Attribute against that block's existing Postgres rows, updating any
// block whose stored pool_tag no longer matches what fresh attribution logic would
// produce. This exists for the situation where internal/poolattr's prefix table (or any
// other part of its attribution logic) changes after blocks have already been indexed
// with the old logic's answer baked into their pool_tag column: re-running the
// indexer's full GRPC backfill just to refresh that one derived column would be
// wasteful (and requires live base-node access) when every input Attribute needs
// (pow_algo_raw, output_count, and the block's coinbase output's coinbase_extra)
// already lives in Postgres from the original index run. This tool never talks to
// GRPC, and never runs migrations - it only reads/updates rows in an already-
// provisioned schema.
//
// Safe to run against a live database with concurrent readers (the HTTP server) and a
// concurrently-running follow-mode indexer: it snapshots MaxIndexedHeight exactly once
// at startup (unless -to is given explicitly) and only ever touches heights up to that
// fixed snapshot, so new blocks the indexer inserts past that point during this tool's
// run are never read or written by it.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Snipa22/go-tari-explorer/internal/config"
	"github.com/Snipa22/go-tari-explorer/internal/db"
	"github.com/Snipa22/go-tari-explorer/internal/poolattr"
)

func main() {
	postgresDSN := flag.String("postgres-dsn", config.PostgresDSN(), "Postgres connection string (env: TARI_EXPLORER_POSTGRES_DSN)")
	batchSize := flag.Uint64("batch-size", 1000, "Number of consecutive block heights to process per batch")
	from := flag.Uint64("from", 0, "Starting height (inclusive)")
	to := flag.Uint64("to", 0, "Ending height (inclusive). 0 (default) means \"max indexed height, captured once at startup\"")
	dryRun := flag.Bool("dry-run", false, "Compute and log what would change, but issue no UPDATE statements")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *batchSize == 0 {
		log.Fatalf("reattribute: -batch-size must be > 0")
	}

	database, err := db.Connect(ctx, *postgresDSN)
	if err != nil {
		log.Fatalf("reattribute: %v", err)
	}
	defer database.Close()

	// Deliberately no database.Migrate(ctx) call here, unlike cmd/indexer/cmd/server -
	// this tool only reads/updates rows in an already-provisioned schema, it never
	// provisions one itself (see package doc comment).

	toHeight := *to
	if toHeight == 0 {
		toHeight, err = database.MaxIndexedHeight(ctx)
		if err != nil {
			log.Fatalf("reattribute: max indexed height: %v", err)
		}
	}
	if *from > toHeight {
		log.Fatalf("reattribute: -from (%d) is past the resolved end height (%d)", *from, toHeight)
	}

	log.Printf("reattribute: reattributing heights %d-%d in batches of %d (dry-run=%t)", *from, toHeight, *batchSize, *dryRun)

	started := time.Now()
	var totalScanned, totalUpdates int64

	for batchFrom := *from; batchFrom <= toHeight; batchFrom += *batchSize {
		if err := ctx.Err(); err != nil {
			log.Fatalf("reattribute: %v", err)
		}

		batchTo := batchFrom + *batchSize - 1
		if batchTo > toHeight {
			batchTo = toHeight
		}

		batchScanned, batchUpdates, err := reattributeBatch(ctx, database, batchFrom, batchTo, *dryRun)
		if err != nil {
			log.Fatalf("reattribute: %v", err)
		}
		totalScanned += int64(batchScanned)
		totalUpdates += int64(batchUpdates)

		log.Printf("reattribute: processed heights %d-%d (%d blocks, %d updates so far)", batchFrom, batchTo, batchScanned, totalUpdates)
	}

	verb := "updated"
	if *dryRun {
		verb = "would update"
	}
	log.Printf("reattribute: done: scanned %d blocks, %s %d, took %s", totalScanned, verb, totalUpdates, time.Since(started))
}

// reattributeBatch processes one [fromHeight, toHeight] batch: it loads the batch's
// candidate block rows and their coinbase-extra data in exactly two queries (see
// db.BlocksInHeightRange / db.CoinbaseExtraForHeightRange), recomputes each block's
// pool_tag via internal/poolattr.Attribute, and either logs (dryRun) or issues (else)
// an UPDATE for every block whose recomputed pool_tag differs from what's currently
// stored. Returns how many blocks were scanned and how many were (or, in dry-run,
// would be) updated.
func reattributeBatch(ctx context.Context, database *db.DB, fromHeight, toHeight uint64, dryRun bool) (scanned, updated int, err error) {
	candidates, err := database.BlocksInHeightRange(ctx, fromHeight, toHeight)
	if err != nil {
		return 0, 0, fmt.Errorf("blocks in height range [%d-%d]: %w", fromHeight, toHeight, err)
	}
	coinbaseExtras, err := database.CoinbaseExtraForHeightRange(ctx, fromHeight, toHeight)
	if err != nil {
		return 0, 0, fmt.Errorf("coinbase extra for height range [%d-%d]: %w", fromHeight, toHeight, err)
	}

	for _, block := range candidates {
		hasOutputs := block.OutputCount > 0
		extra, hasCoinbaseOutput := coinbaseExtras[block.Height]
		// The outputs table only ever stores a row for a block that had real output
		// rows inserted (see internal/indexer.outputRows / ReplaceOutputsForBlock), and
		// a COINBASE (output_type = 1) row existing at all necessarily means that
		// output's OutputFeatures message was non-nil - a nil features would scan as
		// output_type 0 (STANDARD), not 1 (see internal/indexer.outputRows, which reads
		// straight off output.GetFeatures() with no nil guard). So hasFeatures mirrors
		// hasCoinbaseOutput exactly here, matching the same has-features/has-coinbase
		// coupling internal/indexer.indexBlock's own single coinbase-detection loop
		// already assumes.
		hasFeatures := hasCoinbaseOutput

		attribution := poolattr.Attribute(block.Height, block.PowAlgoRaw, hasOutputs, hasCoinbaseOutput, hasFeatures, extra)
		newTag := poolTagFor(attribution)

		if !poolTagChanged(block.PoolTag, newTag) {
			continue
		}

		if dryRun {
			log.Printf("reattribute: [dry-run] height %d: pool_tag %s -> %s", block.Height, formatPoolTag(block.PoolTag), formatPoolTag(newTag))
		} else if err := database.SetPoolTag(ctx, block.Height, newTag); err != nil {
			return len(candidates), updated, fmt.Errorf("set pool tag for block %d: %w", block.Height, err)
		}
		updated++
	}
	return len(candidates), updated, nil
}

// poolTagFor derives the *string to store in the pool_tag column from a
// poolattr.BlockAttribution, matching internal/indexer.indexBlock's own convention:
// BlockAttribution.PoolTag == "" means unattributed (stored as nil/NULL), any non-empty
// PoolTag becomes a pointer to that string.
func poolTagFor(attribution poolattr.BlockAttribution) *string {
	if attribution.PoolTag == "" {
		return nil
	}
	tag := attribution.PoolTag
	return &tag
}

// poolTagChanged reports whether old (the currently-stored pool_tag) and new (the
// freshly recomputed one) differ meaningfully enough to warrant an UPDATE: nil vs nil
// is unchanged (both mean "no pool tag"), nil vs non-nil (or vice versa) is always a
// change, and two non-nil pointers are compared by their underlying string value, never
// by pointer identity.
func poolTagChanged(old, new *string) bool {
	if old == nil && new == nil {
		return false
	}
	if old == nil || new == nil {
		return true
	}
	return *old != *new
}

// formatPoolTag renders a *string pool_tag for log output: nil prints as the literal
// <nil> (rather than an empty string, which could otherwise be misread as an actual
// stored empty-string tag value), and a non-nil tag prints quoted so any
// whitespace/non-printable padding in a real tag (see internal/poolattr's own note on
// own-pool tags like "WUF  Ahri   ") is visible rather than silently swallowed.
func formatPoolTag(tag *string) string {
	if tag == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%q", *tag)
}
