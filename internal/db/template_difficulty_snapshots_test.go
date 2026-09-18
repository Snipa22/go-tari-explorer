package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// templateDifficultyTestDSN returns the Postgres connection string this file's tests
// run against: a dedicated throwaway database, distinct from db_test.go's and
// difficulty_snapshots_test.go's own test databases, so this suite doesn't collide
// with concurrent test runs against the shared embedded Postgres instance. Override
// with TARI_EXPLORER_TEMPLATE_DIFFICULTY_TEST_POSTGRES_DSN in CI or a different local
// setup.
func templateDifficultyTestDSN() string {
	if v := os.Getenv("TARI_EXPLORER_TEMPLATE_DIFFICULTY_TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://postgres@localhost:5433/tari_explorer_template_difficulty_test?sslmode=disable&host=/workspace/pg-embed/sockets"
}

// openTemplateDifficultyTestDB connects to templateDifficultyTestDSN(), runs
// migrations, and truncates template_difficulty_snapshots so each test starts from a
// clean slate. Skips the test (not fails) if the database isn't reachable, matching
// openDifficultyTestDB's convention in difficulty_snapshots_test.go.
func openTemplateDifficultyTestDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()

	d, err := Connect(ctx, templateDifficultyTestDSN())
	if err != nil {
		t.Skipf("db: template difficulty test postgres not reachable at %s: %v", templateDifficultyTestDSN(), err)
	}
	t.Cleanup(d.Close)

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("db: migrate: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `TRUNCATE TABLE template_difficulty_snapshots`); err != nil {
		t.Fatalf("db: truncate: %v", err)
	}
	return d
}

func TestUpsertTemplateDifficultySnapshot_InsertsOncePerAlgoHeight(t *testing.T) {
	d := openTemplateDifficultyTestDB(t)
	ctx := context.Background()

	recordedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	inserted, err := d.UpsertTemplateDifficultySnapshot(ctx, TemplateDifficultySnapshot{
		Algo:             "RXM",
		Height:           1000,
		TargetDifficulty: 12345,
		Reward:           500000,
		RecordedAt:       recordedAt,
	})
	if err != nil {
		t.Fatalf("UpsertTemplateDifficultySnapshot (first): %v", err)
	}
	if !inserted {
		t.Fatal("expected first upsert for a new (algo, height) to report inserted=true")
	}

	// Re-upserting the exact same (algo, height) - simulating a poll tick where the
	// template height hasn't advanced - must be a no-op: no new row, inserted=false,
	// no error, and the original row's values preserved (DO NOTHING).
	inserted, err = d.UpsertTemplateDifficultySnapshot(ctx, TemplateDifficultySnapshot{
		Algo:             "RXM",
		Height:           1000,
		TargetDifficulty: 99999, // even with different values, still a no-op
		Reward:           1,
		RecordedAt:       recordedAt.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("UpsertTemplateDifficultySnapshot (duplicate): %v", err)
	}
	if inserted {
		t.Fatal("expected re-upserting the same (algo, height) to report inserted=false")
	}

	got, err := d.LatestTemplateDifficultySnapshots(ctx)
	if err != nil {
		t.Fatalf("LatestTemplateDifficultySnapshots: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 snapshot row after a duplicate upsert, got %d: %+v", len(got), got)
	}
	if got[0].TargetDifficulty != 12345 || got[0].Reward != 500000 {
		t.Errorf("expected the original target_difficulty 12345/reward 500000 to be preserved (DO NOTHING), got %+v", got[0])
	}
}

func TestLatestTemplateDifficultySnapshots_ReturnsHighestHeightPerAlgo(t *testing.T) {
	d := openTemplateDifficultyTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	rows := []TemplateDifficultySnapshot{
		{Algo: "RXM", Height: 100, TargetDifficulty: 10, Reward: 1, RecordedAt: base},
		{Algo: "RXM", Height: 101, TargetDifficulty: 11, Reward: 2, RecordedAt: base.Add(time.Second)},
		{Algo: "SHA3X", Height: 200, TargetDifficulty: 500, Reward: 3, RecordedAt: base},
	}
	for _, r := range rows {
		if _, err := d.UpsertTemplateDifficultySnapshot(ctx, r); err != nil {
			t.Fatalf("UpsertTemplateDifficultySnapshot: %v", err)
		}
	}

	got, err := d.LatestTemplateDifficultySnapshots(ctx)
	if err != nil {
		t.Fatalf("LatestTemplateDifficultySnapshots: %v", err)
	}
	byAlgo := map[string]TemplateDifficultySnapshot{}
	for _, r := range got {
		byAlgo[r.Algo] = r
	}
	if len(byAlgo) != 2 {
		t.Fatalf("expected 2 algos, got %d: %+v", len(byAlgo), got)
	}
	if rxm := byAlgo["RXM"]; rxm.Height != 101 || rxm.TargetDifficulty != 11 || rxm.Reward != 2 {
		t.Errorf("expected RXM's latest snapshot to be height 101/target_difficulty 11/reward 2, got %+v", rxm)
	}
	if sha := byAlgo["SHA3X"]; sha.Height != 200 || sha.TargetDifficulty != 500 || sha.Reward != 3 {
		t.Errorf("expected SHA3X snapshot height 200/target_difficulty 500/reward 3, got %+v", sha)
	}
}

func TestLatestTemplateDifficultySnapshots_EmptyTable(t *testing.T) {
	d := openTemplateDifficultyTestDB(t)
	ctx := context.Background()

	got, err := d.LatestTemplateDifficultySnapshots(ctx)
	if err != nil {
		t.Fatalf("LatestTemplateDifficultySnapshots: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 snapshots on an empty table, got %d", len(got))
	}
}
