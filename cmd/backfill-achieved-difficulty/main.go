// Command backfill-achieved-difficulty backfills the achieved_difficulty column (see
// internal/db/migrations/0011_achieved_difficulty.up.sql) for already-indexed blocks,
// without re-running the full indexer backfill (which also re-attributes pool tags
// and re-replaces every kernel/output row - unnecessary work just to fill in one
// derived column) and without touching any other column on the `blocks` row. This is
// the one-time historical counterpart to internal/indexer.go's "capture going
// forward" change: achieved_difficulty is the REAL proof-of-work difficulty a
// block's miner actually achieved (BlockHeaderResponse.GetDifficulty(), server-side
// sourced from acc_data.achieved_difficulty), re-derivable for any already-mined
// height via a fresh GetHeaderByHash lookup keyed on that block's own hash - this
// tool just automates doing that for every already-indexed height.
//
// Unlike cmd/backfill-adjusted-difficulty (which fetches a whole [from, to] height
// range in a single GetNetworkDifficulty streaming call), GetHeaderByHash is a unary
// per-hash RPC - there is no ranged/batched variant - so this tool calls it once per
// block in the batch. To avoid re-fetching a full block via GRPC just to read its
// header hash back off the wire, each block's hash is read straight out of Postgres
// (internal/db.DB.HashesForHeightRange), where it's already stored from the original
// index - the batch only ever touches GRPC for the one field (achieved_difficulty)
// Postgres doesn't have yet.
//
// Safe to run against a live database with concurrent readers (the HTTP server) and a
// concurrently-running follow-mode indexer, same rationale as
// cmd/backfill-adjusted-difficulty/cmd/reindex-rewards: it snapshots MaxIndexedHeight
// exactly once at startup (unless -to is given explicitly) and only ever touches
// heights up to that fixed snapshot.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"

	"github.com/Snipa22/go-tari-explorer/internal/config"
	"github.com/Snipa22/go-tari-explorer/internal/db"
	"github.com/Snipa22/go-tari-explorer/internal/nodeclient"
)

// defaultBatchSize is the number of consecutive block heights whose hashes/existing
// achieved_difficulty values are read from Postgres per batch, matching
// internal/indexer.BatchSize/cmd/backfill-adjusted-difficulty's defaultBatchSize - the
// same DB-side batching convention applies here, even though (unlike
// GetNetworkDifficulty) the GRPC side of this tool still issues one GetHeaderByHash
// call per block within that batch (see this file's doc comment).
const defaultBatchSize = 20

func main() {
	postgresDSN := flag.String("postgres-dsn", config.PostgresDSN(), "Postgres connection string (env: TARI_EXPLORER_POSTGRES_DSN)")
	nodeHosts := flag.String("base-node-grpc-hosts", joinHosts(config.NodeGRPCHosts()), "Comma-separated list of base-node GRPC host:port targets")
	batchSize := flag.Uint64("batch-size", defaultBatchSize, "Number of consecutive block heights to process per batch")
	from := flag.Uint64("from", 0, "Starting height (inclusive)")
	to := flag.Uint64("to", 0, "Ending height (inclusive). 0 (default) means \"max indexed height, captured once at startup\"")
	dryRun := flag.Bool("dry-run", false, "Compute and log what would change, but issue no UPDATE statements")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *batchSize == 0 {
		log.Fatalf("backfill-achieved-difficulty: -batch-size must be > 0")
	}

	hosts := config.ParseHostList(*nodeHosts)
	node, err := nodeclient.New(hosts)
	if err != nil {
		log.Fatalf("backfill-achieved-difficulty: %v", err)
	}
	defer node.Close()

	database, err := db.Connect(ctx, *postgresDSN)
	if err != nil {
		log.Fatalf("backfill-achieved-difficulty: %v", err)
	}
	defer database.Close()

	// Deliberately no database.Migrate(ctx) call here, matching
	// cmd/backfill-adjusted-difficulty's own "reads/updates rows in an
	// already-provisioned schema, never provisions one itself" rationale.

	toHeight := *to
	if toHeight == 0 {
		toHeight, err = database.MaxIndexedHeight(ctx)
		if err != nil {
			log.Fatalf("backfill-achieved-difficulty: max indexed height: %v", err)
		}
	}
	if *from > toHeight {
		log.Fatalf("backfill-achieved-difficulty: -from (%d) is past the resolved end height (%d)", *from, toHeight)
	}

	log.Printf("backfill-achieved-difficulty: backfilling achieved difficulty for heights %d-%d in batches of %d across hosts %v (dry-run=%t)", *from, toHeight, *batchSize, hosts, *dryRun)

	started := time.Now()
	var totalScanned, totalUpdates int64

	for batchFrom := *from; batchFrom <= toHeight; batchFrom += *batchSize {
		if err := ctx.Err(); err != nil {
			log.Fatalf("backfill-achieved-difficulty: %v", err)
		}

		batchTo := batchFrom + *batchSize - 1
		if batchTo > toHeight {
			batchTo = toHeight
		}

		batchScanned, batchUpdates, err := backfillAchievedDifficultyBatch(ctx, database, node, batchFrom, batchTo, *dryRun)
		if err != nil {
			log.Fatalf("backfill-achieved-difficulty: %v", err)
		}
		totalScanned += int64(batchScanned)
		totalUpdates += int64(batchUpdates)

		log.Printf("backfill-achieved-difficulty: processed heights %d-%d (%d heights, %d updates so far)", batchFrom, batchTo, batchScanned, totalUpdates)
	}

	verb := "updated"
	if *dryRun {
		verb = "would update"
	}
	log.Printf("backfill-achieved-difficulty: done: scanned %d heights, %s %d, took %s", totalScanned, verb, totalUpdates, time.Since(started))
}

// headerByHashFetcher is the minimal nodeclient.Client surface
// backfillAchievedDifficultyBatch needs - narrowed to an interface so a fake/stub can
// stand in for it in tests without a real GRPC dial, mirroring
// cmd/backfill-adjusted-difficulty's own networkDifficultyFetcher interface rationale.
type headerByHashFetcher interface {
	GetHeaderByHash(ctx context.Context, hash []byte) (*tari_generated.BlockHeaderResponse, error)
}

// backfillAchievedDifficultyBatch processes one [fromHeight, toHeight] batch: it
// reads the batch's already-stored block hashes (database.HashesForHeightRange) and
// currently-stored achieved_difficulty values (database.AchievedDifficultiesForHeightRange)
// from Postgres in one call each, then for every height with a (decodable) stored
// hash, issues one GetHeaderByHash call (node.GetHeaderByHash) - there is no
// ranged/batched variant of this RPC, unlike GetNetworkDifficulty - and either logs
// (dryRun) or issues (database.SetAchievedDifficulty) an update for every height whose
// freshly-fetched value differs from what's currently stored
// (achievedDifficultyChanged). A height whose stored hash fails to hex-decode (should
// not happen for data written by this repo's own indexer, but handled defensively
// rather than aborting the batch) or whose GetHeaderByHash call fails is logged and
// skipped, exactly like internal/indexer.go's own "failed lookup is non-fatal"
// capture contract - it is NOT counted as scanned. Returns how many heights were
// scanned (i.e. actually got a GetHeaderByHash response) and how many were (or, in
// dry-run, would be) updated - a height whose extracted value already matches the
// stored value is left untouched, which is what makes re-running this tool over an
// already-backfilled range a no-op. Mirrors
// cmd/backfill-adjusted-difficulty.backfillAdjustedDifficultyBatch's exact shape,
// adapted for GetHeaderByHash's unary-per-block call shape instead of a single ranged
// streaming call.
func backfillAchievedDifficultyBatch(ctx context.Context, database *db.DB, node headerByHashFetcher, fromHeight, toHeight uint64, dryRun bool) (scanned, updated int, err error) {
	hashes, err := database.HashesForHeightRange(ctx, fromHeight, toHeight)
	if err != nil {
		return 0, 0, fmt.Errorf("hashes for height range [%d-%d]: %w", fromHeight, toHeight, err)
	}
	existing, err := database.AchievedDifficultiesForHeightRange(ctx, fromHeight, toHeight)
	if err != nil {
		return 0, 0, fmt.Errorf("achieved difficulties for height range [%d-%d]: %w", fromHeight, toHeight, err)
	}

	// Iterate in ascending height order (map iteration order is unspecified) so log
	// output reads naturally and test assertions on call order are deterministic.
	heights := make([]uint64, 0, len(hashes))
	for h := range hashes {
		heights = append(heights, h)
	}
	sort.Slice(heights, func(i, j int) bool { return heights[i] < heights[j] })

	for _, height := range heights {
		hashBytes, decodeErr := hex.DecodeString(hashes[height])
		if decodeErr != nil {
			log.Printf("backfill-achieved-difficulty: height %d: malformed stored hash %q, skipping: %v", height, hashes[height], decodeErr)
			continue
		}

		hdr, fetchErr := node.GetHeaderByHash(ctx, hashBytes)
		if fetchErr != nil {
			log.Printf("backfill-achieved-difficulty: height %d: GetHeaderByHash failed, skipping: %v", height, fetchErr)
			continue
		}
		scanned++

		newAchieved := extractAchievedDifficulty(hdr)
		oldAchieved := existing[height] // nil if height isn't present (no row, or a real stored NULL) - both treated as "unset"

		if !achievedDifficultyChanged(oldAchieved, newAchieved) {
			continue
		}

		if dryRun {
			log.Printf("backfill-achieved-difficulty: [dry-run] height %d: achieved_difficulty %s -> %s", height, displayPtr(oldAchieved), displayPtr(newAchieved))
		} else if err := database.SetAchievedDifficulty(ctx, height, newAchieved); err != nil {
			return scanned, updated, fmt.Errorf("set achieved difficulty for block %d: %w", height, err)
		}
		updated++
	}
	return scanned, updated, nil
}

// extractAchievedDifficulty returns hdr's Difficulty as *int64. Unlike
// cmd/backfill-adjusted-difficulty's extractAdjustedDifficulty, there is no
// "field absent on the wire" case to check for here: BlockHeaderResponse.Difficulty
// is a plain (non-optional) proto3 uint64 (see internal/nodeclient.Client.GetHeaderByHash's
// doc comment) - a failed/missing lookup is represented by the GetHeaderByHash call
// itself returning an error (handled one level up in backfillAchievedDifficultyBatch),
// not by a nil-able field on a successful response. This function exists anyway (kept
// as its own named step, mirroring extractAdjustedDifficulty's shape) so the
// uint64->int64 conversion has a single, independently-testable home.
func extractAchievedDifficulty(hdr *tari_generated.BlockHeaderResponse) *int64 {
	v := int64(hdr.GetDifficulty())
	return &v
}

// achievedDifficultyChanged reports whether old (the currently-stored
// achieved_difficulty, nil meaning SQL NULL/unset) and new (the freshly
// re-fetched-from-GRPC value) differ - the sole condition under which
// backfillAchievedDifficultyBatch issues (or, in dry-run, logs) an update. Mirrors
// cmd/backfill-adjusted-difficulty.adjustedDifficultyChanged exactly: nil is never
// conflated with a real 0 on either side, so (nil, nil) is "unchanged", (nil,
// non-nil) or (non-nil, nil) is always "changed" regardless of what the non-nil value
// is, and (non-nil, non-nil) compares the pointed-to values. In practice new is
// always non-nil here (extractAchievedDifficulty never returns nil), but the nil-safe
// comparison is kept for symmetry with the adjusted-difficulty tool and in case that
// ever changes.
func achievedDifficultyChanged(old, new *int64) bool {
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
// "achieved_difficulty NULL -> 193350") instead of printing a Go pointer address or a
// misleading bare "<nil>". Reimplemented here rather than exported/shared with
// cmd/backfill-adjusted-difficulty's identical helper, matching this ecosystem's
// existing convention of small per-command helpers over cross-cmd/ package sharing
// (see that command's own joinHosts doc comment for the same rationale).
func displayPtr(v *int64) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprintf("%d", *v)
}

// joinHosts re-joins an already-parsed default host list into the comma-separated
// string form -base-node-grpc-hosts' flag default expects, matching
// cmd/backfill-adjusted-difficulty's own joinHosts helper exactly (reimplemented here
// rather than exported/shared across cmd/ packages for such a small, self-contained
// helper).
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
