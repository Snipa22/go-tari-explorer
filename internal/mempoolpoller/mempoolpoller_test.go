package mempoolpoller

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// fakeStatsFetcher is a StatsFetcher backed by an in-memory canned response/error, no
// live GRPC connection required - this is the seam that makes Poller.Tick/Run
// unit-testable per this package's doc comment.
type fakeStatsFetcher struct {
	resp *tari_generated.MempoolStatsResponse
	err  error
	// calls counts how many times GetMempoolStats was invoked, so Run-loop tests can
	// assert on tick count without racing on wall-clock timing.
	calls int
}

func (f *fakeStatsFetcher) GetMempoolStats(ctx context.Context) (*tari_generated.MempoolStatsResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// mempoolPollerTestDSN returns the Postgres connection string this file's tests run
// against: a dedicated throwaway database (tari_explorer_mempoolpoller_test), distinct
// from internal/db's own test databases, so this package's tests don't collide with
// concurrent runs against the shared embedded Postgres instance. Override with
// TARI_EXPLORER_MEMPOOLPOLLER_TEST_POSTGRES_DSN in CI or a different local setup.
func mempoolPollerTestDSN() string {
	if v := os.Getenv("TARI_EXPLORER_MEMPOOLPOLLER_TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://postgres@localhost:5433/tari_explorer_mempoolpoller_test?sslmode=disable&host=/workspace/pg-embed/sockets"
}

// openTestDB connects to mempoolPollerTestDSN(), runs migrations, and truncates
// mempool_snapshots so each test starts from a clean slate. Skips the test (not
// fails) if the database isn't reachable, matching internal/db's own test convention.
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()

	d, err := db.Connect(ctx, mempoolPollerTestDSN())
	if err != nil {
		t.Skipf("mempoolpoller: test postgres not reachable at %s: %v", mempoolPollerTestDSN(), err)
	}
	t.Cleanup(d.Close)

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("mempoolpoller: migrate: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `TRUNCATE TABLE mempool_snapshots`); err != nil {
		t.Fatalf("mempoolpoller: truncate: %v", err)
	}
	return d
}

// TestTick_InsertsOneRealRow proves one Tick call against a fake fetcher inserts
// exactly one real mempool_snapshots row into the real (test) Postgres database, with
// the fetched stats and a deterministic (fake-clock) SnapshotTime carried through
// correctly.
func TestTick_InsertsOneRealRow(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	fixedTime := time.Date(2026, 8, 24, 15, 30, 0, 0, time.UTC)
	fetcher := &fakeStatsFetcher{
		resp: &tari_generated.MempoolStatsResponse{
			UnconfirmedTxs:    9,
			ReorgTxs:          1,
			UnconfirmedWeight: 55555,
		},
	}

	poller := New(fetcher, database)
	poller.Now = func() time.Time { return fixedTime }

	if err := poller.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if fetcher.calls != 1 {
		t.Fatalf("expected exactly 1 GetMempoolStats call, got %d", fetcher.calls)
	}

	rows, err := database.ListMempoolSnapshots(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ListMempoolSnapshots: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 row inserted by one Tick, got %d: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.UnconfirmedTxs != 9 || got.ReorgTxs != 1 || got.UnconfirmedWeight != 55555 {
		t.Errorf("unexpected snapshot fields: %+v", got)
	}
	if !got.SnapshotTime.Equal(fixedTime) {
		t.Errorf("expected snapshot_time %v, got %v", fixedTime, got.SnapshotTime)
	}
}

// TestTick_MultipleTicksInsertMultipleRows proves repeated Tick calls each insert
// their own new row rather than upserting/overwriting a previous one.
func TestTick_MultipleTicksInsertMultipleRows(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	fetcher := &fakeStatsFetcher{resp: &tari_generated.MempoolStatsResponse{UnconfirmedTxs: 1}}
	poller := New(fetcher, database)

	for i := 0; i < 3; i++ {
		if err := poller.Tick(ctx); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
	}

	rows, err := database.ListMempoolSnapshots(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ListMempoolSnapshots: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows after 3 ticks, got %d", len(rows))
	}
}

// TestTick_FetchErrorPropagatesWithoutInserting proves a StatsFetcher error is
// surfaced as an error from Tick, and that nothing gets inserted in that case.
func TestTick_FetchErrorPropagatesWithoutInserting(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()

	fetcher := &fakeStatsFetcher{err: errors.New("boom: node unreachable")}
	poller := New(fetcher, database)

	if err := poller.Tick(ctx); err == nil {
		t.Fatal("expected Tick to propagate the fetcher's error")
	}

	rows, err := database.ListMempoolSnapshots(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ListMempoolSnapshots: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected no rows inserted after a failed fetch, got %d", len(rows))
	}
}

// TestRun_TicksUntilContextCancelled proves Run keeps calling Tick on the configured
// interval (inserting one row per tick) until its context is cancelled, then returns
// promptly with ctx.Err() - the same graceful-shutdown contract
// internal/indexer.Follow already provides, exercised here for Poller.Run.
func TestRun_TicksUntilContextCancelled(t *testing.T) {
	database := openTestDB(t)

	fetcher := &fakeStatsFetcher{resp: &tari_generated.MempoolStatsResponse{UnconfirmedTxs: 3}}
	poller := New(fetcher, database)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx, 10*time.Millisecond) }()

	// Let a handful of ticks happen, then cancel and verify Run returns promptly.
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

	rows, err := database.ListMempoolSnapshots(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("ListMempoolSnapshots: %v", err)
	}
	if len(rows) != fetcher.calls {
		t.Fatalf("expected one row per successful tick: %d calls but %d rows", fetcher.calls, len(rows))
	}
}
