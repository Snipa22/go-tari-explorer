package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// mempoolTestDSN returns the Postgres connection string this file's tests run
// against: a dedicated throwaway database, distinct from db_test.go's
// tari_explorer_txcheck_test (and any other feature branch's own test database) so
// this suite doesn't collide with concurrent test runs against the shared embedded
// Postgres instance. Override with TARI_EXPLORER_MEMPOOL_TEST_POSTGRES_DSN in CI or a
// different local setup.
func mempoolTestDSN() string {
	if v := os.Getenv("TARI_EXPLORER_MEMPOOL_TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://postgres@localhost:5433/tari_explorer_mempool_test?sslmode=disable&host=/workspace/pg-embed/sockets"
}

// openMempoolTestDB connects to mempoolTestDSN(), runs migrations, and truncates
// mempool_snapshots so each test starts from a clean slate. Skips the test (not
// fails) if the database isn't reachable, matching openTestDB's convention in
// db_test.go.
func openMempoolTestDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()

	d, err := Connect(ctx, mempoolTestDSN())
	if err != nil {
		t.Skipf("db: mempool test postgres not reachable at %s: %v", mempoolTestDSN(), err)
	}
	t.Cleanup(d.Close)

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("db: migrate: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `TRUNCATE TABLE mempool_snapshots`); err != nil {
		t.Fatalf("db: truncate: %v", err)
	}
	return d
}

func TestInsertMempoolSnapshot_InsertsRow(t *testing.T) {
	d := openMempoolTestDB(t)
	ctx := context.Background()

	snapTime := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	if err := d.InsertMempoolSnapshot(ctx, MempoolSnapshot{
		SnapshotTime:      snapTime,
		UnconfirmedTxs:    10,
		ReorgTxs:          2,
		UnconfirmedWeight: 123456,
	}); err != nil {
		t.Fatalf("InsertMempoolSnapshot: %v", err)
	}

	got, err := d.ListMempoolSnapshots(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ListMempoolSnapshots: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 snapshot, got %d: %+v", len(got), got)
	}
	if got[0].UnconfirmedTxs != 10 || got[0].ReorgTxs != 2 || got[0].UnconfirmedWeight != 123456 {
		t.Errorf("unexpected snapshot: %+v", got[0])
	}
	if !got[0].SnapshotTime.Equal(snapTime) {
		t.Errorf("expected snapshot_time %v, got %v", snapTime, got[0].SnapshotTime)
	}
	if got[0].ID == 0 {
		t.Errorf("expected a non-zero generated id, got %d", got[0].ID)
	}
}

func TestInsertMempoolSnapshot_AllowsDuplicateTimestamps(t *testing.T) {
	d := openMempoolTestDB(t)
	ctx := context.Background()

	snapTime := time.Date(2026, 8, 24, 13, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if err := d.InsertMempoolSnapshot(ctx, MempoolSnapshot{
			SnapshotTime:   snapTime,
			UnconfirmedTxs: int32(i),
		}); err != nil {
			t.Fatalf("InsertMempoolSnapshot (%d): %v", i, err)
		}
	}

	got, err := d.ListMempoolSnapshots(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ListMempoolSnapshots: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 snapshots sharing a timestamp to both be stored, got %d", len(got))
	}
}

func TestListMempoolSnapshots_OrderedBySnapshotTimeAscending(t *testing.T) {
	d := openMempoolTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	// Insert out of order; List must still return ascending by snapshot_time.
	times := []time.Time{base.Add(2 * time.Minute), base, base.Add(1 * time.Minute)}
	for i, ts := range times {
		if err := d.InsertMempoolSnapshot(ctx, MempoolSnapshot{SnapshotTime: ts, UnconfirmedTxs: int32(i)}); err != nil {
			t.Fatalf("InsertMempoolSnapshot: %v", err)
		}
	}

	got, err := d.ListMempoolSnapshots(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ListMempoolSnapshots: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 snapshots, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].SnapshotTime.Before(got[i-1].SnapshotTime) {
			t.Fatalf("expected ascending snapshot_time order, got %v then %v", got[i-1].SnapshotTime, got[i].SnapshotTime)
		}
	}
}

func TestListMempoolSnapshots_FromToRangeFilters(t *testing.T) {
	d := openMempoolTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		ts := base.Add(time.Duration(i) * time.Hour)
		if err := d.InsertMempoolSnapshot(ctx, MempoolSnapshot{SnapshotTime: ts, UnconfirmedTxs: int32(i)}); err != nil {
			t.Fatalf("InsertMempoolSnapshot: %v", err)
		}
	}

	from := base.Add(1 * time.Hour)
	to := base.Add(3 * time.Hour)
	got, err := d.ListMempoolSnapshots(ctx, &from, &to)
	if err != nil {
		t.Fatalf("ListMempoolSnapshots: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 snapshots in [from,to], got %d: %+v", len(got), got)
	}
	for _, s := range got {
		if s.SnapshotTime.Before(from) || s.SnapshotTime.After(to) {
			t.Errorf("snapshot %+v outside requested range [%v, %v]", s, from, to)
		}
	}

	// from only (no upper bound): expect the 3 snapshots at/after `from`.
	gotFromOnly, err := d.ListMempoolSnapshots(ctx, &from, nil)
	if err != nil {
		t.Fatalf("ListMempoolSnapshots (from only): %v", err)
	}
	if len(gotFromOnly) != 4 {
		t.Fatalf("expected 4 snapshots at/after from, got %d", len(gotFromOnly))
	}

	// to only (no lower bound): expect the 4 snapshots at/before `to`.
	gotToOnly, err := d.ListMempoolSnapshots(ctx, nil, &to)
	if err != nil {
		t.Fatalf("ListMempoolSnapshots (to only): %v", err)
	}
	if len(gotToOnly) != 4 {
		t.Fatalf("expected 4 snapshots at/before to, got %d", len(gotToOnly))
	}
}

func TestListMempoolSnapshots_EmptyTable(t *testing.T) {
	d := openMempoolTestDB(t)
	ctx := context.Background()

	got, err := d.ListMempoolSnapshots(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ListMempoolSnapshots: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 snapshots on an empty table, got %d", len(got))
	}
}
