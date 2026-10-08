package main

import (
	"context"
	"os"
	"testing"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// backfillAdjustedDifficultyTestDSN returns the Postgres connection string this
// file's tests run against: a dedicated throwaway database, distinct from every
// other package's own test database (see AGENTS.md / this repo's embedded-pg
// convention), so this suite doesn't collide with concurrent test runs against the
// shared embedded Postgres instance. Override with
// TARI_EXPLORER_BACKFILL_ADJDIFF_TEST_POSTGRES_DSN in CI or a different local setup.
func backfillAdjustedDifficultyTestDSN() string {
	if v := os.Getenv("TARI_EXPLORER_BACKFILL_ADJDIFF_TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://postgres@localhost:5433/tari_explorer_backfill_adjdiff_test?sslmode=disable&host=/workspace/pg-embed/sockets"
}

// openBackfillAdjustedDifficultyTestDB connects to backfillAdjustedDifficultyTestDSN(),
// runs migrations, and truncates `blocks` so each test starts from a clean slate.
// Skips the test (not fails) if the database isn't reachable, matching every other
// DB-backed test suite's convention in this repo.
func openBackfillAdjustedDifficultyTestDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()

	d, err := db.Connect(ctx, backfillAdjustedDifficultyTestDSN())
	if err != nil {
		t.Skipf("backfill-adjusted-difficulty: test postgres not reachable at %s: %v", backfillAdjustedDifficultyTestDSN(), err)
	}
	t.Cleanup(d.Close)

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("backfill-adjusted-difficulty: migrate: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `TRUNCATE TABLE kernels, outputs, block_kernels, blocks CASCADE`); err != nil {
		t.Fatalf("backfill-adjusted-difficulty: truncate: %v", err)
	}
	return d
}

// seedAdjDiffTestBlock inserts a minimal valid `blocks` row at height with the given
// initial difficulty/adjustedDifficulty, so backfillAdjustedDifficultyBatch's
// diff-against-stored-value logic has something real to compare its freshly-"fetched"
// value against.
func seedAdjDiffTestBlock(t *testing.T, d *db.DB, height uint64, difficulty int64, adjustedDifficulty *int64) {
	t.Helper()
	err := d.UpsertBlock(context.Background(), db.Block{
		Height:             height,
		Hash:               "aa",
		PrevHash:           "bb",
		OutputMr:           []byte{},
		BlockOutputMr:      []byte{},
		KernelMr:           []byte{},
		InputMr:            []byte{},
		TotalKernelOffset:  []byte{},
		TotalScriptOffset:  []byte{},
		ValidatorNodeMr:    []byte{},
		PowData:            []byte{},
		PowAlgo:            "RXM",
		Difficulty:         difficulty,
		AdjustedDifficulty: adjustedDifficulty,
	})
	if err != nil {
		t.Fatalf("backfill-adjusted-difficulty: seed block %d: %v", height, err)
	}
}

// fakeNetworkDifficultyFetcher implements networkDifficultyFetcher without any real
// GRPC dial, returning a fixed set of responses regardless of the requested range -
// the batch-processing logic under test (backfillAdjustedDifficultyBatch) only ever
// calls GetNetworkDifficulty once per batch and doesn't care whether the result came
// from a real base node or this fixture, so a plain interface stub is sufficient here
// (no bufconn fake server needed, unlike internal/nodeclient's own tests, which exist
// specifically to exercise the real dial/failover/stream-draining code that lives one
// layer below this interface).
type fakeNetworkDifficultyFetcher struct {
	responses []*tari_generated.NetworkDifficultyResponse
}

func (f *fakeNetworkDifficultyFetcher) GetNetworkDifficulty(_ context.Context, fromHeight, toHeight uint64) ([]*tari_generated.NetworkDifficultyResponse, error) {
	var out []*tari_generated.NetworkDifficultyResponse
	for _, r := range f.responses {
		if h := r.GetHeight(); h >= fromHeight && h <= toHeight {
			out = append(out, r)
		}
	}
	return out, nil
}

func diffResponse(height, difficulty uint64, adjustedDifficulty *uint64) *tari_generated.NetworkDifficultyResponse {
	return &tari_generated.NetworkDifficultyResponse{Height: height, Difficulty: difficulty, AdjustedDifficulty: adjustedDifficulty}
}

func u64ptr(v uint64) *uint64 { return &v }
func i64ptr(v int64) *int64   { return &v }

// TestBackfillAdjustedDifficultyBatch_DryRunDoesNotWrite proves a -dry-run batch
// reports the updates it would make (via its return values) without actually issuing
// any UPDATE - the stored adjusted_difficulty for every touched height must be
// completely unchanged after a dry run.
func TestBackfillAdjustedDifficultyBatch_DryRunDoesNotWrite(t *testing.T) {
	database := openBackfillAdjustedDifficultyTestDB(t)
	ctx := context.Background()

	seedAdjDiffTestBlock(t, database, 100, 10_000, nil) // not yet backfilled
	seedAdjDiffTestBlock(t, database, 101, 20_000, nil)

	fetcher := &fakeNetworkDifficultyFetcher{responses: []*tari_generated.NetworkDifficultyResponse{
		diffResponse(100, 10_000, u64ptr(320_000)),
		diffResponse(101, 20_000, u64ptr(640_000)),
	}}

	scanned, updated, err := backfillAdjustedDifficultyBatch(ctx, database, fetcher, 100, 101, true)
	if err != nil {
		t.Fatalf("backfillAdjustedDifficultyBatch: %v", err)
	}
	if scanned != 2 {
		t.Errorf("scanned = %d, want 2", scanned)
	}
	if updated != 2 {
		t.Errorf("updated = %d, want 2 (both heights differ from stored NULL)", updated)
	}

	// Nothing should actually have been written.
	got, err := database.AdjustedDifficultiesForHeightRange(ctx, 100, 101)
	if err != nil {
		t.Fatalf("AdjustedDifficultiesForHeightRange: %v", err)
	}
	if got[100] != nil || got[101] != nil {
		t.Fatalf("dry-run must not write: got %+v, want both nil", got)
	}
}

// TestBackfillAdjustedDifficultyBatch_RealRunWritesThenIsIdempotent proves a real
// (non-dry-run) batch actually writes the freshly-fetched adjusted difficulty
// values, and that running the exact same batch again against the exact same fetched
// data is a no-op (0 updates, matching the task's idempotency requirement).
func TestBackfillAdjustedDifficultyBatch_RealRunWritesThenIsIdempotent(t *testing.T) {
	database := openBackfillAdjustedDifficultyTestDB(t)
	ctx := context.Background()

	seedAdjDiffTestBlock(t, database, 200, 10_000, nil)
	seedAdjDiffTestBlock(t, database, 201, 20_000, nil)

	fetcher := &fakeNetworkDifficultyFetcher{responses: []*tari_generated.NetworkDifficultyResponse{
		diffResponse(200, 10_000, u64ptr(320_000)),
		diffResponse(201, 20_000, nil), // simulates a host that didn't return the field for this height
	}}

	scanned, updated, err := backfillAdjustedDifficultyBatch(ctx, database, fetcher, 200, 201, false)
	if err != nil {
		t.Fatalf("backfillAdjustedDifficultyBatch (real run): %v", err)
	}
	if scanned != 2 {
		t.Errorf("scanned = %d, want 2", scanned)
	}
	if updated != 1 {
		t.Errorf("updated = %d, want 1 (only height 200 actually changed from NULL)", updated)
	}

	got, err := database.AdjustedDifficultiesForHeightRange(ctx, 200, 201)
	if err != nil {
		t.Fatalf("AdjustedDifficultiesForHeightRange: %v", err)
	}
	if got[200] == nil || *got[200] != 320_000 {
		t.Errorf("height 200 adjusted difficulty = %+v, want 320000", got[200])
	}
	if got[201] != nil {
		t.Errorf("height 201 adjusted difficulty = %v, want nil (no field returned, stays NULL)", *got[201])
	}

	// Re-running the exact same batch against the exact same fetched data must be a
	// no-op: every height's freshly-fetched value now matches what's already stored.
	scanned2, updated2, err := backfillAdjustedDifficultyBatch(ctx, database, fetcher, 200, 201, false)
	if err != nil {
		t.Fatalf("backfillAdjustedDifficultyBatch (re-run): %v", err)
	}
	if scanned2 != 2 {
		t.Errorf("re-run scanned = %d, want 2", scanned2)
	}
	if updated2 != 0 {
		t.Errorf("re-run updated = %d, want 0 (idempotent no-op)", updated2)
	}
}

// TestBackfillAdjustedDifficultyBatch_MissingHeightIsSkipped proves a height the
// fetcher has no response for (simulating a GRPC response that didn't include every
// requested height) is simply not scanned/updated, rather than causing a crash or a
// spurious write.
func TestBackfillAdjustedDifficultyBatch_MissingHeightIsSkipped(t *testing.T) {
	database := openBackfillAdjustedDifficultyTestDB(t)
	ctx := context.Background()

	seedAdjDiffTestBlock(t, database, 300, 1_000, nil)
	seedAdjDiffTestBlock(t, database, 301, 2_000, nil)

	fetcher := &fakeNetworkDifficultyFetcher{responses: []*tari_generated.NetworkDifficultyResponse{
		diffResponse(300, 1_000, u64ptr(32_000)),
		// 301 deliberately absent.
	}}

	scanned, updated, err := backfillAdjustedDifficultyBatch(ctx, database, fetcher, 300, 301, false)
	if err != nil {
		t.Fatalf("backfillAdjustedDifficultyBatch: %v", err)
	}
	if scanned != 1 {
		t.Errorf("scanned = %d, want 1 (only height 300 was returned)", scanned)
	}
	if updated != 1 {
		t.Errorf("updated = %d, want 1", updated)
	}

	got, err := database.AdjustedDifficultiesForHeightRange(ctx, 300, 301)
	if err != nil {
		t.Fatalf("AdjustedDifficultiesForHeightRange: %v", err)
	}
	if got[300] == nil || *got[300] != 32_000 {
		t.Errorf("height 300 adjusted difficulty = %+v, want 32000", got[300])
	}
	if got[301] != nil {
		t.Errorf("height 301 (never fetched) adjusted difficulty = %v, want unchanged nil", *got[301])
	}
}

// TestBackfillAdjustedDifficultyBatch_AlreadyCapturedValueIsLeftUntouched proves a
// height whose freshly-fetched adjusted difficulty EXACTLY matches what's already
// stored (including both being a real non-nil value) is not re-written - the
// no-op-when-unchanged half of the idempotency contract, distinct from the
// nil-vs-non-nil cases the other tests cover.
func TestBackfillAdjustedDifficultyBatch_AlreadyCapturedValueIsLeftUntouched(t *testing.T) {
	database := openBackfillAdjustedDifficultyTestDB(t)
	ctx := context.Background()

	seedAdjDiffTestBlock(t, database, 400, 1_000, i64ptr(32_000))

	fetcher := &fakeNetworkDifficultyFetcher{responses: []*tari_generated.NetworkDifficultyResponse{
		diffResponse(400, 1_000, u64ptr(32_000)), // identical to what's already stored
	}}

	scanned, updated, err := backfillAdjustedDifficultyBatch(ctx, database, fetcher, 400, 400, false)
	if err != nil {
		t.Fatalf("backfillAdjustedDifficultyBatch: %v", err)
	}
	if scanned != 1 {
		t.Errorf("scanned = %d, want 1", scanned)
	}
	if updated != 0 {
		t.Errorf("updated = %d, want 0 (value already matches stored)", updated)
	}
}
