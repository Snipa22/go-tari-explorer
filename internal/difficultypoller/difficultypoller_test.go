package difficultypoller

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// fakeFetcher is a Fetcher backed by an in-memory canned response/error, no real
// Postgres read required for exercising Tick's fetch side - this is the seam that
// makes Poller.Tick/Run unit-testable per this package's doc comment. The upsert side
// still goes through a real (test) Postgres connection.
type fakeFetcher struct {
	rows []db.CurrentDifficultyRow
	err  error
	// calls counts how many times CurrentDifficultyPerAlgo was invoked, so Run-loop
	// tests can assert on tick count without racing on wall-clock timing.
	calls int
}

func (f *fakeFetcher) CurrentDifficultyPerAlgo(ctx context.Context) ([]db.CurrentDifficultyRow, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// difficultyPollerTestDSN returns the Postgres connection string this file's tests run
// against: a dedicated throwaway database (tari_explorer_difficultypoller_test),
// distinct from internal/db's own test databases, so this package's tests don't
// collide with concurrent runs against the shared embedded Postgres instance. Override
// with TARI_EXPLORER_DIFFICULTYPOLLER_TEST_POSTGRES_DSN in CI or a different local
// setup.
func difficultyPollerTestDSN() string {
	if v := os.Getenv("TARI_EXPLORER_DIFFICULTYPOLLER_TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://postgres@localhost:5433/tari_explorer_difficultypoller_test?sslmode=disable&host=/workspace/pg-embed/sockets"
}

// openTestDB connects to difficultyPollerTestDSN(), runs migrations, and truncates
// difficulty_snapshots so each test starts from a clean slate. Skips the test (not
// fails) if the database isn't reachable, matching internal/db's own test convention.
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()

	d, err := db.Connect(ctx, difficultyPollerTestDSN())
	if err != nil {
		t.Skipf("difficultypoller: test postgres not reachable at %s: %v", difficultyPollerTestDSN(), err)
	}
	t.Cleanup(d.Close)

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("difficultypoller: migrate: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `TRUNCATE TABLE difficulty_snapshots`); err != nil {
		t.Fatalf("difficultypoller: truncate: %v", err)
	}
	return d
}

// TestTick_InsertsOneRowPerNewAlgoHeight proves one Tick call, given 2 algos' current
// (height, difficulty), inserts exactly one new real difficulty_snapshots row per algo.
func TestTick_InsertsOneRowPerNewAlgoHeight(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	fixedTime := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fetcher := &fakeFetcher{
		rows: []db.CurrentDifficultyRow{
			{Algo: "RXM", Height: 1000, Difficulty: 111},
			{Algo: "SHA3X", Height: 2000, Difficulty: 222},
		},
	}

	poller := New(fetcher, database)
	poller.Now = func() time.Time { return fixedTime }

	n, err := poller.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 rows inserted on first observation of 2 algos, got %d", n)
	}
	if fetcher.calls != 1 {
		t.Fatalf("expected exactly 1 fetch call, got %d", fetcher.calls)
	}

	rows, err := database.LatestDifficultySnapshots(ctx)
	if err != nil {
		t.Fatalf("LatestDifficultySnapshots: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 snapshot rows, got %d: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if !r.RecordedAt.Equal(fixedTime) {
			t.Errorf("expected RecordedAt %v, got %v for %+v", fixedTime, r.RecordedAt, r)
		}
	}
}

// TestTick_RepeatedTicksWithNoHeightChangeInsertNothing proves the common case -
// ticking repeatedly while an algo's height hasn't advanced - inserts no new rows
// after the first observation, matching the "one row per real block, not per tick"
// contract from migrations/0007_difficulty_snapshots.up.sql.
func TestTick_RepeatedTicksWithNoHeightChangeInsertNothing(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	fetcher := &fakeFetcher{rows: []db.CurrentDifficultyRow{{Algo: "RXM", Height: 1000, Difficulty: 111}}}
	poller := New(fetcher, database)

	firstN, err := poller.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick (1st): %v", err)
	}
	if firstN != 1 {
		t.Fatalf("expected 1 row inserted on first tick, got %d", firstN)
	}

	for i := 0; i < 3; i++ {
		n, err := poller.Tick(ctx)
		if err != nil {
			t.Fatalf("Tick (repeat %d): %v", i, err)
		}
		if n != 0 {
			t.Fatalf("expected 0 rows inserted on repeat tick %d (no height change), got %d", i, n)
		}
	}

	rows, err := database.LatestDifficultySnapshots(ctx)
	if err != nil {
		t.Fatalf("LatestDifficultySnapshots: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 total snapshot row after 4 ticks with no height change, got %d", len(rows))
	}
}

// TestTick_HeightAdvanceInsertsANewRow proves that once an algo's height actually
// advances between ticks, the next tick inserts a second row for that algo (not an
// update/overwrite of the first).
func TestTick_HeightAdvanceInsertsANewRow(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	fetcher := &fakeFetcher{rows: []db.CurrentDifficultyRow{{Algo: "RXM", Height: 1000, Difficulty: 111}}}
	poller := New(fetcher, database)

	if _, err := poller.Tick(ctx); err != nil {
		t.Fatalf("Tick (1st): %v", err)
	}

	fetcher.rows = []db.CurrentDifficultyRow{{Algo: "RXM", Height: 1001, Difficulty: 222}}
	n, err := poller.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick (2nd): %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 new row inserted after a real height advance, got %d", n)
	}

	latest, err := database.LatestDifficultySnapshots(ctx)
	if err != nil {
		t.Fatalf("LatestDifficultySnapshots: %v", err)
	}
	if len(latest) != 1 || latest[0].Height != 1001 || latest[0].Difficulty != 222 {
		t.Fatalf("expected latest snapshot to reflect the advanced height/difficulty, got %+v", latest)
	}
}

// TestTick_FetchErrorPropagatesWithoutInserting proves a Fetcher error is surfaced as
// an error from Tick, and that nothing gets inserted in that case.
func TestTick_FetchErrorPropagatesWithoutInserting(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	fetcher := &fakeFetcher{err: errors.New("boom: db unreachable")}
	poller := New(fetcher, database)

	if _, err := poller.Tick(ctx); err == nil {
		t.Fatal("expected Tick to propagate the fetcher's error")
	}

	rows, err := database.LatestDifficultySnapshots(ctx)
	if err != nil {
		t.Fatalf("LatestDifficultySnapshots: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected no rows inserted after a failed fetch, got %d", len(rows))
	}
}

// TestRun_TicksUntilContextCancelled proves Run keeps calling Tick on the configured
// interval until its context is cancelled, then returns promptly with ctx.Err() - the
// same graceful-shutdown contract internal/mempoolpoller.Poller.Run already provides.
func TestRun_TicksUntilContextCancelled(t *testing.T) {
	database := openTestDB(t)

	fetcher := &fakeFetcher{rows: []db.CurrentDifficultyRow{{Algo: "RXM", Height: 1000, Difficulty: 111}}}
	poller := New(fetcher, database)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx, 10*time.Millisecond) }()

	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected Run to return context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of context cancellation")
	}

	if fetcher.calls == 0 {
		t.Fatal("expected at least one tick to have run before cancellation")
	}

	// Only the first tick should have actually inserted a row (unchanged height on
	// every subsequent tick), regardless of how many ticks fetcher.calls counted.
	rows, err := database.LatestDifficultySnapshots(context.Background())
	if err != nil {
		t.Fatalf("LatestDifficultySnapshots: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 snapshot row despite %d ticks (height never changed), got %d", fetcher.calls, len(rows))
	}
}
