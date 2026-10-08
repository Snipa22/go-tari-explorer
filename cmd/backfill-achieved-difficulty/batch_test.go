package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// errNotFound is the sentinel error fakeHeaderByHashFetcher returns for a hash it has
// no response (or explicit error) registered for, simulating a base node's
// GetHeaderByHash call failing for an unknown/missing hash.
var errNotFound = errors.New("fake: header not found for hash")

// backfillAchievedDifficultyTestDSN returns the Postgres connection string this
// file's tests run against: a dedicated throwaway database, distinct from every
// other package's own test database (see AGENTS.md / this repo's embedded-pg
// convention), so this suite doesn't collide with concurrent test runs against the
// shared embedded Postgres instance. Override with
// TARI_EXPLORER_BACKFILL_ACHDIFF_TEST_POSTGRES_DSN in CI or a different local setup.
func backfillAchievedDifficultyTestDSN() string {
	if v := os.Getenv("TARI_EXPLORER_BACKFILL_ACHDIFF_TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://postgres@localhost:5433/tari_explorer_backfill_achdiff_test?sslmode=disable&host=/workspace/pg-embed/sockets"
}

// openBackfillAchievedDifficultyTestDB connects to backfillAchievedDifficultyTestDSN(),
// runs migrations, and truncates `blocks` so each test starts from a clean slate.
// Skips the test (not fails) if the database isn't reachable, matching every other
// DB-backed test suite's convention in this repo.
func openBackfillAchievedDifficultyTestDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()

	d, err := db.Connect(ctx, backfillAchievedDifficultyTestDSN())
	if err != nil {
		t.Skipf("backfill-achieved-difficulty: test postgres not reachable at %s: %v", backfillAchievedDifficultyTestDSN(), err)
	}
	t.Cleanup(d.Close)

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("backfill-achieved-difficulty: migrate: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `TRUNCATE TABLE kernels, outputs, block_kernels, blocks CASCADE`); err != nil {
		t.Fatalf("backfill-achieved-difficulty: truncate: %v", err)
	}
	return d
}

// seedAchDiffTestBlock inserts a minimal valid `blocks` row at height with the given
// hash/initial difficulty/achievedDifficulty, so backfillAchievedDifficultyBatch's
// hash-lookup and diff-against-stored-value logic both have something real to work
// against.
func seedAchDiffTestBlock(t *testing.T, d *db.DB, height uint64, hash string, difficulty int64, achievedDifficulty *int64) {
	t.Helper()
	err := d.UpsertBlock(context.Background(), db.Block{
		Height:             height,
		Hash:               hash,
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
		AchievedDifficulty: achievedDifficulty,
	})
	if err != nil {
		t.Fatalf("backfill-achieved-difficulty: seed block %d: %v", height, err)
	}
}

// fakeHeaderByHashFetcher implements headerByHashFetcher without any real GRPC dial,
// keyed by hex-encoded hash - the batch-processing logic under test
// (backfillAchievedDifficultyBatch) only ever calls GetHeaderByHash once per height
// and doesn't care whether the result came from a real base node or this fixture, so
// a plain interface stub is sufficient here (no bufconn fake server needed, unlike
// internal/nodeclient's own tests, which exist specifically to exercise the real
// dial/failover/wire-call code that lives one layer below this interface).
type fakeHeaderByHashFetcher struct {
	responses map[string]*tari_generated.BlockHeaderResponse // key: hex-encoded hash
	errs      map[string]error                               // key: hex-encoded hash; takes precedence over responses
}

func (f *fakeHeaderByHashFetcher) GetHeaderByHash(_ context.Context, hash []byte) (*tari_generated.BlockHeaderResponse, error) {
	key := fmt.Sprintf("%x", hash)
	if err, ok := f.errs[key]; ok {
		return nil, err
	}
	resp, ok := f.responses[key]
	if !ok {
		return nil, errNotFound
	}
	return resp, nil
}

func i64ptr(v int64) *int64 { return &v }

// TestBackfillAchievedDifficultyBatch_DryRunDoesNotWrite proves a -dry-run batch
// reports the updates it would make (via its return values) without actually issuing
// any UPDATE - the stored achieved_difficulty for every touched height must be
// completely unchanged after a dry run.
func TestBackfillAchievedDifficultyBatch_DryRunDoesNotWrite(t *testing.T) {
	database := openBackfillAchievedDifficultyTestDB(t)
	ctx := context.Background()

	seedAchDiffTestBlock(t, database, 100, "aa", 50_000, nil) // not yet backfilled
	seedAchDiffTestBlock(t, database, 101, "bb", 60_000, nil)

	fetcher := &fakeHeaderByHashFetcher{responses: map[string]*tari_generated.BlockHeaderResponse{
		"aa": {Difficulty: 193_350},
		"bb": {Difficulty: 210_000},
	}}

	scanned, updated, err := backfillAchievedDifficultyBatch(ctx, database, fetcher, 100, 101, true)
	if err != nil {
		t.Fatalf("backfillAchievedDifficultyBatch: %v", err)
	}
	if scanned != 2 {
		t.Errorf("scanned = %d, want 2", scanned)
	}
	if updated != 2 {
		t.Errorf("updated = %d, want 2 (both heights differ from stored NULL)", updated)
	}

	// Nothing should actually have been written.
	got, err := database.AchievedDifficultiesForHeightRange(ctx, 100, 101)
	if err != nil {
		t.Fatalf("AchievedDifficultiesForHeightRange: %v", err)
	}
	if got[100] != nil || got[101] != nil {
		t.Fatalf("dry-run must not write: got %+v, want both nil", got)
	}
}

// TestBackfillAchievedDifficultyBatch_RealRunWritesThenIsIdempotent proves a real
// (non-dry-run) batch actually writes the freshly-fetched achieved difficulty
// values, and that running the exact same batch again against the exact same fetched
// data is a no-op (0 updates, matching the task's idempotency requirement).
func TestBackfillAchievedDifficultyBatch_RealRunWritesThenIsIdempotent(t *testing.T) {
	database := openBackfillAchievedDifficultyTestDB(t)
	ctx := context.Background()

	seedAchDiffTestBlock(t, database, 200, "cc", 50_000, nil)
	seedAchDiffTestBlock(t, database, 201, "dd", 60_000, nil)

	fetcher := &fakeHeaderByHashFetcher{responses: map[string]*tari_generated.BlockHeaderResponse{
		"cc": {Difficulty: 193_350},
		"dd": {Difficulty: 210_000},
	}}

	scanned, updated, err := backfillAchievedDifficultyBatch(ctx, database, fetcher, 200, 201, false)
	if err != nil {
		t.Fatalf("backfillAchievedDifficultyBatch (real run): %v", err)
	}
	if scanned != 2 {
		t.Errorf("scanned = %d, want 2", scanned)
	}
	if updated != 2 {
		t.Errorf("updated = %d, want 2", updated)
	}

	got, err := database.AchievedDifficultiesForHeightRange(ctx, 200, 201)
	if err != nil {
		t.Fatalf("AchievedDifficultiesForHeightRange: %v", err)
	}
	if got[200] == nil || *got[200] != 193_350 {
		t.Errorf("height 200 achieved difficulty = %+v, want 193350", got[200])
	}
	if got[201] == nil || *got[201] != 210_000 {
		t.Errorf("height 201 achieved difficulty = %+v, want 210000", got[201])
	}

	// Re-running the exact same batch against the exact same fetched data must be a
	// no-op: every height's freshly-fetched value now matches what's already stored.
	scanned2, updated2, err := backfillAchievedDifficultyBatch(ctx, database, fetcher, 200, 201, false)
	if err != nil {
		t.Fatalf("backfillAchievedDifficultyBatch (re-run): %v", err)
	}
	if scanned2 != 2 {
		t.Errorf("re-run scanned = %d, want 2", scanned2)
	}
	if updated2 != 0 {
		t.Errorf("re-run updated = %d, want 0 (idempotent no-op)", updated2)
	}
}

// TestBackfillAchievedDifficultyBatch_FailedLookupIsSkippedNotScanned proves a height
// whose GetHeaderByHash call errors (simulating an unknown hash, host error, etc.) is
// simply not scanned/updated, rather than causing a crash, a spurious write, or
// aborting the whole batch - the same non-fatal-on-failure contract
// internal/indexer.go's own capture has.
func TestBackfillAchievedDifficultyBatch_FailedLookupIsSkippedNotScanned(t *testing.T) {
	database := openBackfillAchievedDifficultyTestDB(t)
	ctx := context.Background()

	seedAchDiffTestBlock(t, database, 300, "ee", 10_000, nil)
	seedAchDiffTestBlock(t, database, 301, "ff", 20_000, nil)

	fetcher := &fakeHeaderByHashFetcher{
		responses: map[string]*tari_generated.BlockHeaderResponse{
			"ee": {Difficulty: 32_000},
		},
		errs: map[string]error{
			"ff": errNotFound, // simulates a failed/missing lookup for this hash
		},
	}

	scanned, updated, err := backfillAchievedDifficultyBatch(ctx, database, fetcher, 300, 301, false)
	if err != nil {
		t.Fatalf("backfillAchievedDifficultyBatch: %v", err)
	}
	if scanned != 1 {
		t.Errorf("scanned = %d, want 1 (only height 300's lookup succeeded)", scanned)
	}
	if updated != 1 {
		t.Errorf("updated = %d, want 1", updated)
	}

	got, err := database.AchievedDifficultiesForHeightRange(ctx, 300, 301)
	if err != nil {
		t.Fatalf("AchievedDifficultiesForHeightRange: %v", err)
	}
	if got[300] == nil || *got[300] != 32_000 {
		t.Errorf("height 300 achieved difficulty = %+v, want 32000", got[300])
	}
	if got[301] != nil {
		t.Errorf("height 301 (failed lookup) achieved difficulty = %v, want unchanged nil", *got[301])
	}
}

// TestBackfillAchievedDifficultyBatch_AlreadyCapturedValueIsLeftUntouched proves a
// height whose freshly-fetched achieved difficulty EXACTLY matches what's already
// stored is not re-written - the no-op-when-unchanged half of the idempotency
// contract, distinct from the nil-vs-non-nil case the other tests cover.
func TestBackfillAchievedDifficultyBatch_AlreadyCapturedValueIsLeftUntouched(t *testing.T) {
	database := openBackfillAchievedDifficultyTestDB(t)
	ctx := context.Background()

	seedAchDiffTestBlock(t, database, 400, "11", 1_000, i64ptr(32_000))

	fetcher := &fakeHeaderByHashFetcher{responses: map[string]*tari_generated.BlockHeaderResponse{
		"11": {Difficulty: 32_000}, // identical to what's already stored
	}}

	scanned, updated, err := backfillAchievedDifficultyBatch(ctx, database, fetcher, 400, 400, false)
	if err != nil {
		t.Fatalf("backfillAchievedDifficultyBatch: %v", err)
	}
	if scanned != 1 {
		t.Errorf("scanned = %d, want 1", scanned)
	}
	if updated != 0 {
		t.Errorf("updated = %d, want 0 (value already matches stored)", updated)
	}
}

// TestBackfillAchievedDifficultyBatch_MalformedStoredHashIsSkipped proves a height
// whose stored hash is not valid hex (should not happen for data written by this
// repo's own indexer, but handled defensively) is logged and skipped rather than
// crashing the batch.
func TestBackfillAchievedDifficultyBatch_MalformedStoredHashIsSkipped(t *testing.T) {
	database := openBackfillAchievedDifficultyTestDB(t)
	ctx := context.Background()

	seedAchDiffTestBlock(t, database, 500, "not-valid-hex!!", 10_000, nil)

	fetcher := &fakeHeaderByHashFetcher{responses: map[string]*tari_generated.BlockHeaderResponse{}}

	scanned, updated, err := backfillAchievedDifficultyBatch(ctx, database, fetcher, 500, 500, false)
	if err != nil {
		t.Fatalf("backfillAchievedDifficultyBatch: %v", err)
	}
	if scanned != 0 {
		t.Errorf("scanned = %d, want 0 (malformed hash never reaches GetHeaderByHash)", scanned)
	}
	if updated != 0 {
		t.Errorf("updated = %d, want 0", updated)
	}
}
