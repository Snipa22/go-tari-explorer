// Command backfill-adjusted-difficulty backfills the adjusted_difficulty column (see
// internal/db/migrations/0010_adjusted_difficulty.up.sql) for already-indexed blocks,
// without re-running the full indexer backfill (which also re-attributes pool tags
// and re-replaces every kernel/output row - unnecessary work just to fill in one
// derived column) and without touching any other column on the `blocks` row. This is
// the one-time historical counterpart to internal/indexer.go's "capture going
// forward" change: AdjustedDifficulty is populated per-block by the base node (not
// just for the live tip), so it can be re-derived for any already-mined height by
// re-querying GetNetworkDifficulty for that height - this tool just automates doing
// that for every already-indexed height.
//
// Unlike cmd/reattribute (which only ever reads/updates rows already in Postgres),
// this tool DOES need to talk to GRPC, mirroring cmd/reindex-rewards' own rationale
// for the same reason. Unlike reindex-rewards (which re-fetches full blocks via
// GetBlockByHeight to extract a coinbase field), this tool calls
// internal/nodeclient.Client.GetNetworkDifficulty with a real [from, to] height range
// per batch (HeightRequest.StartHeight/EndHeight, already supported by the
// GetNetworkDifficulty streaming RPC - see that method's doc comment) rather than one
// GRPC call per height, which is both cheaper and simpler than looping
// batch-size-many single-height calls.
//
// Safe to run against a live database with concurrent readers (the HTTP server) and a
// concurrently-running follow-mode indexer, same rationale as cmd/reattribute/
// cmd/reindex-rewards: it snapshots MaxIndexedHeight exactly once at startup (unless
// -to is given explicitly) and only ever touches heights up to that fixed snapshot.
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

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"

	"github.com/Snipa22/go-tari-explorer/internal/config"
	"github.com/Snipa22/go-tari-explorer/internal/db"
	"github.com/Snipa22/go-tari-explorer/internal/nodeclient"
)

// defaultBatchSize is the number of consecutive block heights requested per
// GetNetworkDifficulty range call, matching internal/indexer.BatchSize/
// cmd/reindex-rewards' defaultBatchSize - the same GRPC-side batching convention
// applies here, just backing a single ranged call instead of a slice of individual
// height lookups.
const defaultBatchSize = 20

func main() {
	postgresDSN := flag.String("postgres-dsn", config.PostgresDSN(), "Postgres connection string (env: TARI_EXPLORER_POSTGRES_DSN)")
	nodeHosts := flag.String("base-node-grpc-hosts", joinHosts(config.NodeGRPCHosts()), "Comma-separated list of base-node GRPC host:port targets")
	batchSize := flag.Uint64("batch-size", defaultBatchSize, "Number of consecutive block heights to fetch per GRPC batch")
	from := flag.Uint64("from", 0, "Starting height (inclusive)")
	to := flag.Uint64("to", 0, "Ending height (inclusive). 0 (default) means \"max indexed height, captured once at startup\"")
	dryRun := flag.Bool("dry-run", false, "Compute and log what would change, but issue no UPDATE statements")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *batchSize == 0 {
		log.Fatalf("backfill-adjusted-difficulty: -batch-size must be > 0")
	}

	hosts := config.ParseHostList(*nodeHosts)
	node, err := nodeclient.New(hosts)
	if err != nil {
		log.Fatalf("backfill-adjusted-difficulty: %v", err)
	}
	defer node.Close()

	database, err := db.Connect(ctx, *postgresDSN)
	if err != nil {
		log.Fatalf("backfill-adjusted-difficulty: %v", err)
	}
	defer database.Close()

	// Deliberately no database.Migrate(ctx) call here, matching cmd/reattribute's /
	// cmd/reindex-rewards' own "reads/updates rows in an already-provisioned schema,
	// never provisions one itself" rationale.

	toHeight := *to
	if toHeight == 0 {
		toHeight, err = database.MaxIndexedHeight(ctx)
		if err != nil {
			log.Fatalf("backfill-adjusted-difficulty: max indexed height: %v", err)
		}
	}
	if *from > toHeight {
		log.Fatalf("backfill-adjusted-difficulty: -from (%d) is past the resolved end height (%d)", *from, toHeight)
	}

	log.Printf("backfill-adjusted-difficulty: backfilling adjusted difficulty for heights %d-%d in batches of %d across hosts %v (dry-run=%t)", *from, toHeight, *batchSize, hosts, *dryRun)

	started := time.Now()
	var totalScanned, totalUpdates int64

	for batchFrom := *from; batchFrom <= toHeight; batchFrom += *batchSize {
		if err := ctx.Err(); err != nil {
			log.Fatalf("backfill-adjusted-difficulty: %v", err)
		}

		batchTo := batchFrom + *batchSize - 1
		if batchTo > toHeight {
			batchTo = toHeight
		}

		batchScanned, batchUpdates, err := backfillAdjustedDifficultyBatch(ctx, database, node, batchFrom, batchTo, *dryRun)
		if err != nil {
			log.Fatalf("backfill-adjusted-difficulty: %v", err)
		}
		totalScanned += int64(batchScanned)
		totalUpdates += int64(batchUpdates)

		log.Printf("backfill-adjusted-difficulty: processed heights %d-%d (%d heights, %d updates so far)", batchFrom, batchTo, batchScanned, totalUpdates)
	}

	verb := "updated"
	if *dryRun {
		verb = "would update"
	}
	log.Printf("backfill-adjusted-difficulty: done: scanned %d heights, %s %d, took %s", totalScanned, verb, totalUpdates, time.Since(started))
}

// networkDifficultyFetcher is the minimal nodeclient.Client surface
// backfillAdjustedDifficultyBatch needs - narrowed to an interface so a fake/stub can
// stand in for it in tests without a real GRPC dial, mirroring cmd/reindex-rewards'
// own blockFetcher interface rationale exactly.
type networkDifficultyFetcher interface {
	GetNetworkDifficulty(ctx context.Context, fromHeight, toHeight uint64) ([]*tari_generated.NetworkDifficultyResponse, error)
}

// backfillAdjustedDifficultyBatch processes one [fromHeight, toHeight] batch: it
// fetches the batch's network-difficulty responses from GRPC in a single ranged call
// (node.GetNetworkDifficulty) and the batch's currently-stored adjusted_difficulty
// values from Postgres (database.AdjustedDifficultiesForHeightRange) in one call
// each, then for every fetched response extracts its AdjustedDifficulty
// (extractAdjustedDifficulty) and either logs (dryRun) or issues
// (database.SetAdjustedDifficulty) an update for every height whose
// freshly-fetched value differs from what's currently stored
// (adjustedDifficultyChanged). Returns how many heights were scanned (i.e. actually
// had a GetNetworkDifficulty response) and how many were (or, in dry-run, would be)
// updated - a height whose extracted value already matches the stored value is left
// untouched, which is what makes re-running this tool over an already-backfilled
// range a no-op. Mirrors cmd/reindex-rewards.reindexRewardsBatch's exact shape.
func backfillAdjustedDifficultyBatch(ctx context.Context, database *db.DB, node networkDifficultyFetcher, fromHeight, toHeight uint64, dryRun bool) (scanned, updated int, err error) {
	diffs, err := node.GetNetworkDifficulty(ctx, fromHeight, toHeight)
	if err != nil {
		return 0, 0, fmt.Errorf("get network difficulty [%d-%d]: %w", fromHeight, toHeight, err)
	}
	existing, err := database.AdjustedDifficultiesForHeightRange(ctx, fromHeight, toHeight)
	if err != nil {
		return 0, 0, fmt.Errorf("adjusted difficulties for height range [%d-%d]: %w", fromHeight, toHeight, err)
	}

	for _, diff := range diffs {
		height := diff.GetHeight()
		newAdjusted := extractAdjustedDifficulty(diff)
		oldAdjusted := existing[height] // nil if height isn't present (no row, or a real stored NULL) - both treated as "unset"

		if !adjustedDifficultyChanged(oldAdjusted, newAdjusted) {
			continue
		}

		if dryRun {
			log.Printf("backfill-adjusted-difficulty: [dry-run] height %d: adjusted_difficulty %s -> %s", height, displayPtr(oldAdjusted), displayPtr(newAdjusted))
		} else if err := database.SetAdjustedDifficulty(ctx, height, newAdjusted); err != nil {
			return len(diffs), updated, fmt.Errorf("set adjusted difficulty for block %d: %w", height, err)
		}
		updated++
	}
	return len(diffs), updated, nil
}

// extractAdjustedDifficulty returns diff's AdjustedDifficulty as *int64, or nil if the
// response didn't carry one (a proto3 `optional` field absent on the wire - e.g. the
// responding base-node host predates the TIP-004 field, see
// migrations/0010_adjusted_difficulty.up.sql). Deliberately checks the raw
// AdjustedDifficulty *uint64 field rather than calling GetAdjustedDifficulty()
// (which nil-safely returns 0, indistinguishable from a real captured 0) - this
// function exists specifically to preserve that distinction one layer up from the
// generated proto accessor.
func extractAdjustedDifficulty(diff *tari_generated.NetworkDifficultyResponse) *int64 {
	if diff.AdjustedDifficulty == nil {
		return nil
	}
	v := int64(diff.GetAdjustedDifficulty())
	return &v
}

// adjustedDifficultyChanged reports whether old (the currently-stored
// adjusted_difficulty, nil meaning SQL NULL/unset) and new (the freshly
// re-fetched-from-GRPC value, nil meaning the node didn't return one) differ -
// the sole condition under which backfillAdjustedDifficultyBatch issues (or, in
// dry-run, logs) an update. Unlike cmd/reindex-rewards' rewardChanged (a plain
// uint64 comparison - 0 is always a real, meaningful reward_micro_minotari value),
// this must explicitly handle the nil-vs-non-nil case: nil is never conflated with a
// real 0 on either side (see migrations/0010_adjusted_difficulty.up.sql for the full
// rationale), so (nil, nil) is "unchanged", (nil, non-nil) or (non-nil, nil) is always
// "changed" regardless of what the non-nil value is, and (non-nil, non-nil) compares
// the pointed-to values.
func adjustedDifficultyChanged(old, new *int64) bool {
	if old == nil && new == nil {
		return false
	}
	if (old == nil) != (new == nil) {
		return true
	}
	return *old != *new
}

// displayPtr renders a *int64 for a dry-run log line: "NULL" for nil, the plain
// decimal value otherwise - so a dry-run log line reads naturally (e.g.
// "adjusted_difficulty NULL -> 320000") instead of printing a Go pointer address or a
// misleading bare "<nil>".
func displayPtr(v *int64) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprintf("%d", *v)
}

// joinHosts re-joins an already-parsed default host list into the comma-separated
// string form -base-node-grpc-hosts' flag default expects, matching
// cmd/reindex-rewards' own joinHosts helper exactly (reimplemented here rather than
// exported/shared across cmd/ packages for such a small, self-contained helper).
func joinHosts(hosts []string) string {
	joined := ""
	for i, h := range hosts {
		if i > 0 {
			joined += ","
		}
		joined += h
	}
	return joined
}
