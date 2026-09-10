// Command reindex-rewards backfills the reward_micro_minotari column (see
// internal/db/migrations/0008_reward_micro_minotari.up.sql) for already-indexed
// blocks, without re-running the full indexer backfill (which also re-attributes pool
// tags and re-replaces every kernel/output row - unnecessary work just to fill in one
// derived column) and without touching any other column on the `blocks` row.
//
// Unlike cmd/reattribute (which only ever reads/updates rows already in Postgres),
// this tool DOES need to talk to GRPC: reward_micro_minotari's source value - a
// coinbase output's TransactionOutput.minimum_value_promise - was never captured by
// any indexer run before this feature existed (see the migration's doc comment), so
// there is nothing already in Postgres to re-derive it from. This mirrors
// internal/indexer.go's own GetBlockByHeight batching pattern (same nodeclient
// multi-host client, same BatchSize-shaped request chunking) to re-fetch just enough
// of each block (its coinbase output) to extract that one field.
//
// Safe to run against a live database with concurrent readers (the HTTP server) and a
// concurrently-running follow-mode indexer, same rationale as cmd/reattribute: it
// snapshots MaxIndexedHeight exactly once at startup (unless -to is given explicitly)
// and only ever touches heights up to that fixed snapshot.
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

// batchSize is the number of block heights requested per GetBlockByHeight call,
// matching internal/indexer.BatchSize - the same GRPC-side batching limit applies
// here since this tool re-fetches full blocks (not the trimmed coinbase-extra
// projection cmd/reattribute reads straight out of Postgres).
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
		log.Fatalf("reindex-rewards: -batch-size must be > 0")
	}

	hosts := config.ParseHostList(*nodeHosts)
	node, err := nodeclient.New(hosts)
	if err != nil {
		log.Fatalf("reindex-rewards: %v", err)
	}
	defer node.Close()

	database, err := db.Connect(ctx, *postgresDSN)
	if err != nil {
		log.Fatalf("reindex-rewards: %v", err)
	}
	defer database.Close()

	// Deliberately no database.Migrate(ctx) call here, matching cmd/reattribute's own
	// "reads/updates rows in an already-provisioned schema, never provisions one
	// itself" rationale.

	toHeight := *to
	if toHeight == 0 {
		toHeight, err = database.MaxIndexedHeight(ctx)
		if err != nil {
			log.Fatalf("reindex-rewards: max indexed height: %v", err)
		}
	}
	if *from > toHeight {
		log.Fatalf("reindex-rewards: -from (%d) is past the resolved end height (%d)", *from, toHeight)
	}

	log.Printf("reindex-rewards: reindexing rewards for heights %d-%d in batches of %d across hosts %v (dry-run=%t)", *from, toHeight, *batchSize, hosts, *dryRun)

	started := time.Now()
	var totalScanned, totalUpdates int64

	for batchFrom := *from; batchFrom <= toHeight; batchFrom += *batchSize {
		if err := ctx.Err(); err != nil {
			log.Fatalf("reindex-rewards: %v", err)
		}

		batchTo := batchFrom + *batchSize - 1
		if batchTo > toHeight {
			batchTo = toHeight
		}

		batchScanned, batchUpdates, err := reindexRewardsBatch(ctx, database, node, batchFrom, batchTo, *dryRun)
		if err != nil {
			log.Fatalf("reindex-rewards: %v", err)
		}
		totalScanned += int64(batchScanned)
		totalUpdates += int64(batchUpdates)

		log.Printf("reindex-rewards: processed heights %d-%d (%d blocks, %d updates so far)", batchFrom, batchTo, batchScanned, totalUpdates)
	}

	verb := "updated"
	if *dryRun {
		verb = "would update"
	}
	log.Printf("reindex-rewards: done: scanned %d blocks, %s %d, took %s", totalScanned, verb, totalUpdates, time.Since(started))
}

// blockFetcher is the minimal nodeclient.Client surface reindexRewardsBatch needs -
// narrowed to an interface so a fake/stub can stand in for it in tests without a real
// GRPC dial (mirrors the same "narrow interface for testability" rationale used
// elsewhere in this ecosystem's test suites, e.g. internal/nodeclient's own
// bufconn-backed fake server tests exercise the concrete *nodeclient.Client instead -
// here a plain interface is simpler since this tool only needs this one method).
type blockFetcher interface {
	GetBlockByHeight(ctx context.Context, heights []uint64) ([]*tari_generated.Block, error)
}

// reindexRewardsBatch processes one [fromHeight, toHeight] batch: it fetches the
// batch's blocks from GRPC (node.GetBlockByHeight) and the batch's currently-stored
// reward values from Postgres (database.RewardsForHeightRange) in one call each, then
// for every fetched block extracts its coinbase's minimum_value_promise
// (extractRewardMicroMinotari) and either logs (dryRun) or issues
// (database.SetRewardMicroMinotari) an update for every height whose freshly-fetched
// reward differs from what's currently stored. Returns how many blocks were scanned
// and how many were (or, in dry-run, would be) updated - a block whose extracted
// reward already matches the stored value is left untouched, which is what makes
// re-running this tool over an already-backfilled range a no-op.
func reindexRewardsBatch(ctx context.Context, database *db.DB, node blockFetcher, fromHeight, toHeight uint64, dryRun bool) (scanned, updated int, err error) {
	heights := makeRange(fromHeight, toHeight)
	blocks, err := node.GetBlockByHeight(ctx, heights)
	if err != nil {
		return 0, 0, fmt.Errorf("get blocks [%d-%d]: %w", fromHeight, toHeight, err)
	}
	existing, err := database.RewardsForHeightRange(ctx, fromHeight, toHeight)
	if err != nil {
		return 0, 0, fmt.Errorf("rewards for height range [%d-%d]: %w", fromHeight, toHeight, err)
	}

	for _, block := range blocks {
		height := block.GetHeader().GetHeight()
		newReward := extractRewardMicroMinotari(block)
		oldReward := existing[height] // zero value if height isn't present, which is a real, meaningful "0" (see RewardsForHeightRange)

		if !rewardChanged(oldReward, newReward) {
			continue
		}

		if dryRun {
			log.Printf("reindex-rewards: [dry-run] height %d: reward_micro_minotari %d -> %d", height, oldReward, newReward)
		} else if err := database.SetRewardMicroMinotari(ctx, height, newReward); err != nil {
			return len(blocks), updated, fmt.Errorf("set reward micro minotari for block %d: %w", height, err)
		}
		updated++
	}
	return len(blocks), updated, nil
}

// extractRewardMicroMinotari finds a block's single COINBASE output (mirroring
// internal/indexer.go's indexBlock own "first coinbase output found, break" loop
// exactly) and returns its minimum_value_promise - the raw MicroMinotari reward+fees
// value in cleartext whenever that coinbase used RangeProofType::RevealedValue (see
// migrations/0008_reward_micro_minotari.up.sql for the full derivation). Returns 0 if
// the block has no coinbase output at all (should not happen for a real mined block,
// but handled gracefully rather than panicking) - indistinguishable from a real
// BulletProofPlus-hidden reward, by design (see the migration's doc comment on why 0
// means "unknown", not "no reward").
func extractRewardMicroMinotari(block *tari_generated.Block) uint64 {
	for _, output := range block.GetBody().GetOutputs() {
		features := output.GetFeatures()
		if features == nil {
			continue
		}
		if features.GetOutputType() != uint32(tari_generated.OutputType_COINBASE) {
			continue
		}
		return output.GetMinimumValuePromise()
	}
	return 0
}

// rewardChanged reports whether old (the currently-stored reward_micro_minotari) and
// new (the freshly re-fetched-from-GRPC value) differ - the sole condition under which
// reindexRewardsBatch issues (or, in dry-run, logs) an update. Trivial today (a plain
// uint64 comparison, unlike cmd/reattribute's poolTagChanged which has to handle
// nil-vs-non-nil *string cases) but kept as its own named function for the same
// "isolate the diffing decision from the batch-processing loop" reason poolTagChanged
// exists, and so it has its own direct unit test coverage independent of a real GRPC
// call.
func rewardChanged(old, new uint64) bool {
	return old != new
}

// makeRange returns every uint64 in [min, max] inclusive, ascending - the same shape
// internal/indexer.go's own (unexported) makeRange builds for its GetBlockByHeight
// calls, reimplemented here rather than exported/shared across packages for such a
// small, self-contained helper.
func makeRange(min, max uint64) []uint64 {
	a := make([]uint64, max-min+1)
	for i := range a {
		a[i] = min + uint64(i)
	}
	return a
}

// joinHosts re-joins an already-parsed default host list into the comma-separated
// string form -base-node-grpc-hosts' flag default expects, matching cmd/indexer's own
// envOrJoin helper's join half (this tool doesn't need envOrJoin's env-var-precedence
// half since config.NodeGRPCHosts() already resolves TARI_EXPLORER_NODE_HOSTS itself).
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
