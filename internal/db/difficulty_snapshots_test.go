package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// difficultyTestDSN returns the Postgres connection string this file's tests run
// against: a dedicated throwaway database, distinct from db_test.go's and
// mempool_snapshots_test.go's own test databases, so this suite doesn't collide with
// concurrent test runs against the shared embedded Postgres instance. Override with
// TARI_EXPLORER_DIFFICULTY_TEST_POSTGRES_DSN in CI or a different local setup.
func difficultyTestDSN() string {
	if v := os.Getenv("TARI_EXPLORER_DIFFICULTY_TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://postgres@localhost:5433/tari_explorer_difficulty_test?sslmode=disable&host=/workspace/pg-embed/sockets"
}

// openDifficultyTestDB connects to difficultyTestDSN(), runs migrations, and truncates
// both blocks and difficulty_snapshots so each test starts from a clean slate. Skips
// the test (not fails) if the database isn't reachable, matching openTestDB's
// convention in db_test.go.
func openDifficultyTestDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()

	d, err := Connect(ctx, difficultyTestDSN())
	if err != nil {
		t.Skipf("db: difficulty test postgres not reachable at %s: %v", difficultyTestDSN(), err)
	}
	t.Cleanup(d.Close)

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("db: migrate: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `TRUNCATE TABLE kernels, outputs, block_kernels, blocks, difficulty_snapshots CASCADE`); err != nil {
		t.Fatalf("db: truncate: %v", err)
	}
	return d
}

func TestCurrentDifficultyPerAlgo_PicksHighestHeightPerAlgo(t *testing.T) {
	d := openDifficultyTestDB(t)
	ctx := context.Background()

	// Two RXM blocks (999 then 1001, seeded out of order) and one SHA3X block; the
	// higher-height RXM row must win, and C29/RXT (never seeded) must simply be
	// absent rather than appearing as a fake zero-difficulty row.
	seedBlockFull(t, d, 999, "RXM", nil, 100)
	seedBlockFull(t, d, 1001, "RXM", nil, 300)
	seedBlockFull(t, d, 1000, "SHA3X", nil, 5000)

	got, err := d.CurrentDifficultyPerAlgo(ctx)
	if err != nil {
		t.Fatalf("CurrentDifficultyPerAlgo: %v", err)
	}
	byAlgo := map[string]CurrentDifficultyRow{}
	for _, r := range got {
		byAlgo[r.Algo] = r
	}
	if len(byAlgo) != 2 {
		t.Fatalf("expected exactly 2 algos present, got %d: %+v", len(byAlgo), got)
	}
	if rxm := byAlgo["RXM"]; rxm.Height != 1001 || rxm.Difficulty != 300 {
		t.Errorf("expected RXM to report height 1001/difficulty 300 (the higher-height row), got %+v", rxm)
	}
	if sha := byAlgo["SHA3X"]; sha.Height != 1000 || sha.Difficulty != 5000 {
		t.Errorf("expected SHA3X height 1000/difficulty 5000, got %+v", sha)
	}
	if _, ok := byAlgo["C29"]; ok {
		t.Errorf("expected C29 (never seeded) to be absent, got %+v", byAlgo["C29"])
	}
}

func TestUpsertDifficultySnapshot_InsertsOncePerAlgoHeight(t *testing.T) {
	d := openDifficultyTestDB(t)
	ctx := context.Background()

	recordedAt := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	inserted, err := d.UpsertDifficultySnapshot(ctx, DifficultySnapshot{
		Algo:       "RXM",
		Height:     1000,
		Difficulty: 12345,
		RecordedAt: recordedAt,
	})
	if err != nil {
		t.Fatalf("UpsertDifficultySnapshot (first): %v", err)
	}
	if !inserted {
		t.Fatal("expected first upsert for a new (algo, height) to report inserted=true")
	}

	// Re-upserting the exact same (algo, height) - simulating a poll tick where
	// nothing changed - must be a no-op: no new row, inserted=false, no error.
	inserted, err = d.UpsertDifficultySnapshot(ctx, DifficultySnapshot{
		Algo:       "RXM",
		Height:     1000,
		Difficulty: 99999, // even with a different difficulty value, still a no-op
		RecordedAt: recordedAt.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("UpsertDifficultySnapshot (duplicate): %v", err)
	}
	if inserted {
		t.Fatal("expected re-upserting the same (algo, height) to report inserted=false")
	}

	got, err := d.LatestDifficultySnapshots(ctx)
	if err != nil {
		t.Fatalf("LatestDifficultySnapshots: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 snapshot row after a duplicate upsert, got %d: %+v", len(got), got)
	}
	if got[0].Difficulty != 12345 {
		t.Errorf("expected the original difficulty 12345 to be preserved (DO NOTHING), got %d", got[0].Difficulty)
	}
}

func TestLatestDifficultySnapshots_ReturnsHighestHeightPerAlgo(t *testing.T) {
	d := openDifficultyTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	rows := []DifficultySnapshot{
		{Algo: "RXM", Height: 100, Difficulty: 10, RecordedAt: base},
		{Algo: "RXM", Height: 101, Difficulty: 11, RecordedAt: base.Add(time.Second)},
		{Algo: "SHA3X", Height: 200, Difficulty: 500, RecordedAt: base},
	}
	for _, r := range rows {
		if _, err := d.UpsertDifficultySnapshot(ctx, r); err != nil {
			t.Fatalf("UpsertDifficultySnapshot: %v", err)
		}
	}

	got, err := d.LatestDifficultySnapshots(ctx)
	if err != nil {
		t.Fatalf("LatestDifficultySnapshots: %v", err)
	}
	byAlgo := map[string]DifficultySnapshot{}
	for _, r := range got {
		byAlgo[r.Algo] = r
	}
	if len(byAlgo) != 2 {
		t.Fatalf("expected 2 algos, got %d: %+v", len(byAlgo), got)
	}
	if rxm := byAlgo["RXM"]; rxm.Height != 101 || rxm.Difficulty != 11 {
		t.Errorf("expected RXM's latest snapshot to be height 101/difficulty 11, got %+v", rxm)
	}
	if sha := byAlgo["SHA3X"]; sha.Height != 200 || sha.Difficulty != 500 {
		t.Errorf("expected SHA3X snapshot height 200/difficulty 500, got %+v", sha)
	}
}

func TestLatestDifficultySnapshots_EmptyTable(t *testing.T) {
	d := openDifficultyTestDB(t)
	ctx := context.Background()

	got, err := d.LatestDifficultySnapshots(ctx)
	if err != nil {
		t.Fatalf("LatestDifficultySnapshots: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 snapshots on an empty table, got %d", len(got))
	}
}
