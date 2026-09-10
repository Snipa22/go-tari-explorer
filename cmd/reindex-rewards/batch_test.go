package main

import (
	"context"
	"os"
	"testing"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// rewardsTestDSN returns the Postgres connection string this file's tests run against:
// a dedicated throwaway database, distinct from every other package's own test
// database (see AGENTS.md / this repo's embedded-pg convention), so this suite doesn't
// collide with concurrent test runs against the shared embedded Postgres instance.
// Override with TARI_EXPLORER_REWARDS_TEST_POSTGRES_DSN in CI or a different local
// setup.
func rewardsTestDSN() string {
	if v := os.Getenv("TARI_EXPLORER_REWARDS_TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://postgres@localhost:5433/tari_explorer_rewards_test?sslmode=disable&host=/workspace/pg-embed/sockets"
}

// openRewardsTestDB connects to rewardsTestDSN(), runs migrations, and truncates
// `blocks` so each test starts from a clean slate. Skips the test (not fails) if the
// database isn't reachable, matching every other DB-backed test suite's convention in
// this repo.
func openRewardsTestDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()

	d, err := db.Connect(ctx, rewardsTestDSN())
	if err != nil {
		t.Skipf("reindex-rewards: test postgres not reachable at %s: %v", rewardsTestDSN(), err)
	}
	t.Cleanup(d.Close)

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("reindex-rewards: migrate: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `TRUNCATE TABLE kernels, outputs, block_kernels, blocks CASCADE`); err != nil {
		t.Fatalf("reindex-rewards: truncate: %v", err)
	}
	return d
}

// seedRewardsTestBlock inserts a minimal valid `blocks` row at height with the given
// initial reward_micro_minotari, so reindexRewardsBatch's diff-against-stored-value
// logic has something real to compare its freshly-"fetched" value against.
func seedRewardsTestBlock(t *testing.T, d *db.DB, height uint64, initialReward uint64) {
	t.Helper()
	err := d.UpsertBlock(context.Background(), db.Block{
		Height:              height,
		Hash:                "aa",
		PrevHash:            "bb",
		OutputMr:            []byte{},
		BlockOutputMr:       []byte{},
		KernelMr:            []byte{},
		InputMr:             []byte{},
		TotalKernelOffset:   []byte{},
		TotalScriptOffset:   []byte{},
		ValidatorNodeMr:     []byte{},
		PowData:             []byte{},
		PowAlgo:             "RXM",
		RewardMicroMinotari: initialReward,
	})
	if err != nil {
		t.Fatalf("reindex-rewards: seed block %d: %v", height, err)
	}
}

// fakeBlockFetcher implements blockFetcher without any real GRPC dial, returning a
// fixed set of blocks keyed by height - the batch-processing logic under test
// (reindexRewardsBatch) only ever calls GetBlockByHeight once per batch and doesn't
// care whether the result came from a real base node or this fixture map, so a plain
// interface stub is sufficient here (no bufconn fake server needed, unlike
// internal/nodeclient's own tests, which exist specifically to exercise the real
// dial/failover/stream-draining code that lives one layer below this interface).
type fakeBlockFetcher struct {
	blocksByHeight map[uint64]*tari_generated.Block
}

func (f *fakeBlockFetcher) GetBlockByHeight(_ context.Context, heights []uint64) ([]*tari_generated.Block, error) {
	var out []*tari_generated.Block
	for _, h := range heights {
		if b, ok := f.blocksByHeight[h]; ok {
			out = append(out, b)
		}
	}
	return out, nil
}

func coinbaseBlock(height uint64, minimumValuePromise uint64) *tari_generated.Block {
	return &tari_generated.Block{
		Header: &tari_generated.BlockHeader{Height: height},
		Body: &tari_generated.AggregateBody{
			Outputs: []*tari_generated.TransactionOutput{
				{
					Features:            &tari_generated.OutputFeatures{OutputType: uint32(tari_generated.OutputType_COINBASE)},
					MinimumValuePromise: minimumValuePromise,
				},
			},
		},
	}
}

// TestReindexRewardsBatch_DryRunDoesNotWrite proves a -dry-run batch reports the
// updates it would make (via its return values) without actually issuing any UPDATE -
// the stored reward_micro_minotari for every touched height must be completely
// unchanged after a dry run.
func TestReindexRewardsBatch_DryRunDoesNotWrite(t *testing.T) {
	database := openRewardsTestDB(t)
	ctx := context.Background()

	seedRewardsTestBlock(t, database, 100, 0) // not yet backfilled
	seedRewardsTestBlock(t, database, 101, 0)

	fetcher := &fakeBlockFetcher{blocksByHeight: map[uint64]*tari_generated.Block{
		100: coinbaseBlock(100, 5_000_000_000),
		101: coinbaseBlock(101, 6_000_000_000),
	}}

	scanned, updated, err := reindexRewardsBatch(ctx, database, fetcher, 100, 101, true)
	if err != nil {
		t.Fatalf("reindexRewardsBatch: %v", err)
	}
	if scanned != 2 {
		t.Errorf("scanned = %d, want 2", scanned)
	}
	if updated != 2 {
		t.Errorf("updated = %d, want 2 (both heights differ from stored 0)", updated)
	}

	// Nothing should actually have been written.
	rewards, err := database.RewardsForHeightRange(ctx, 100, 101)
	if err != nil {
		t.Fatalf("RewardsForHeightRange: %v", err)
	}
	if rewards[100] != 0 || rewards[101] != 0 {
		t.Fatalf("dry-run must not write: got rewards %+v, want both 0", rewards)
	}
}

// TestReindexRewardsBatch_RealRunWritesThenIsIdempotent proves a real (non-dry-run)
// batch actually writes the freshly-fetched reward values, and that running the exact
// same batch again against the exact same fetched data is a no-op (0 updates,
// matching the task's idempotency requirement).
func TestReindexRewardsBatch_RealRunWritesThenIsIdempotent(t *testing.T) {
	database := openRewardsTestDB(t)
	ctx := context.Background()

	seedRewardsTestBlock(t, database, 200, 0)
	seedRewardsTestBlock(t, database, 201, 0)

	fetcher := &fakeBlockFetcher{blocksByHeight: map[uint64]*tari_generated.Block{
		200: coinbaseBlock(200, 5_000_000_000),
		201: coinbaseBlock(201, 0), // BulletProofPlus-hidden - genuinely 0, must stay 0
	}}

	scanned, updated, err := reindexRewardsBatch(ctx, database, fetcher, 200, 201, false)
	if err != nil {
		t.Fatalf("reindexRewardsBatch (real run): %v", err)
	}
	if scanned != 2 {
		t.Errorf("scanned = %d, want 2", scanned)
	}
	if updated != 1 {
		t.Errorf("updated = %d, want 1 (only height 200 actually changed from 0)", updated)
	}

	rewards, err := database.RewardsForHeightRange(ctx, 200, 201)
	if err != nil {
		t.Fatalf("RewardsForHeightRange: %v", err)
	}
	if rewards[200] != 5_000_000_000 {
		t.Errorf("height 200 reward = %d, want 5000000000", rewards[200])
	}
	if rewards[201] != 0 {
		t.Errorf("height 201 reward = %d, want 0", rewards[201])
	}

	// Re-running the exact same batch against the exact same fetched data must be a
	// no-op: every height's freshly-fetched reward now matches what's already stored.
	scanned2, updated2, err := reindexRewardsBatch(ctx, database, fetcher, 200, 201, false)
	if err != nil {
		t.Fatalf("reindexRewardsBatch (re-run): %v", err)
	}
	if scanned2 != 2 {
		t.Errorf("re-run scanned = %d, want 2", scanned2)
	}
	if updated2 != 0 {
		t.Errorf("re-run updated = %d, want 0 (idempotent no-op)", updated2)
	}
}

// TestReindexRewardsBatch_MissingHeightIsSkipped proves a height the fetcher has no
// data for (simulating a GRPC response that didn't include every requested height) is
// simply not scanned/updated, rather than causing a crash or a spurious write.
func TestReindexRewardsBatch_MissingHeightIsSkipped(t *testing.T) {
	database := openRewardsTestDB(t)
	ctx := context.Background()

	seedRewardsTestBlock(t, database, 300, 0)
	seedRewardsTestBlock(t, database, 301, 0)

	fetcher := &fakeBlockFetcher{blocksByHeight: map[uint64]*tari_generated.Block{
		300: coinbaseBlock(300, 1_000_000),
		// 301 deliberately absent.
	}}

	scanned, updated, err := reindexRewardsBatch(ctx, database, fetcher, 300, 301, false)
	if err != nil {
		t.Fatalf("reindexRewardsBatch: %v", err)
	}
	if scanned != 1 {
		t.Errorf("scanned = %d, want 1 (only height 300 was returned)", scanned)
	}
	if updated != 1 {
		t.Errorf("updated = %d, want 1", updated)
	}

	rewards, err := database.RewardsForHeightRange(ctx, 300, 301)
	if err != nil {
		t.Fatalf("RewardsForHeightRange: %v", err)
	}
	if rewards[300] != 1_000_000 {
		t.Errorf("height 300 reward = %d, want 1000000", rewards[300])
	}
	if rewards[301] != 0 {
		t.Errorf("height 301 (never fetched) reward = %d, want unchanged 0", rewards[301])
	}
}
