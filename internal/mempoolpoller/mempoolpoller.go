// Package mempoolpoller implements the tick-and-insert loop behind cmd/mempool-poller:
// on a configurable interval, fetch the base node's live aggregate mempool stats
// (GetMempoolStats, via internal/nodeclient) and persist exactly one new
// mempool_snapshots row per tick (via internal/db).
//
// The two dependencies Poller needs - fetching stats, inserting a row - are each
// expressed as a small interface (StatsFetcher, SnapshotInserter) rather than a
// concrete *nodeclient.Client/*db.DB type, the same seam pattern
// internal/txsearch.NodeSearcher/DBSearcher already use in this repo. That makes the
// tick/insert loop itself unit-testable against a fake StatsFetcher - no live GRPC
// connection required - while still exercising a real Postgres insert; see
// mempoolpoller_test.go, and internal/nodeclient's own bufconn-backed tests for the
// live-GRPC-wire-level coverage of GetMempoolStats that this package intentionally
// doesn't re-test.
package mempoolpoller

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// StatsFetcher is the subset of *internal/nodeclient.Client's methods Poller needs.
// Satisfied by *nodeclient.Client; declared here so tests can substitute a fake
// fetcher instead of dialing a real base node.
type StatsFetcher interface {
	GetMempoolStats(ctx context.Context) (*tari_generated.MempoolStatsResponse, error)
}

// SnapshotInserter is the subset of *internal/db.DB's methods Poller needs. Satisfied
// by *db.DB; declared here for the same fake-substitution reason as StatsFetcher,
// though this package's own tests exercise a real Postgres connection (see
// mempoolpoller_test.go) since proving InsertMempoolSnapshot's SQL actually persists a
// row end-to-end is the point of that test.
type SnapshotInserter interface {
	InsertMempoolSnapshot(ctx context.Context, s db.MempoolSnapshot) error
}

// compile-time assertion that *db.DB satisfies SnapshotInserter (the interface exists
// for testability, per this package's doc comment, but production code always wires up
// the real thing).
var _ SnapshotInserter = (*db.DB)(nil)

// Poller bundles the live-node and DB dependencies Tick/Run need.
type Poller struct {
	Stats StatsFetcher
	DB    SnapshotInserter

	// Now returns the current time, used as each snapshot's SnapshotTime. Defaults to
	// time.Now (via the now() helper below) when left nil, as New leaves it - exists
	// purely so tests can assert against a known, deterministic SnapshotTime instead
	// of a moving target.
	Now func() time.Time
}

// New constructs a Poller. Now is left nil (defaults to time.Now via the now() helper);
// set p.Now directly after construction if a test needs a fixed clock.
func New(stats StatsFetcher, inserter SnapshotInserter) *Poller {
	return &Poller{Stats: stats, DB: inserter}
}

// now returns p.Now() if set, else time.Now().UTC().
func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

// Tick fetches the base node's current mempool stats and inserts exactly one new
// mempool_snapshots row for them, timestamped with p.now(). This is the unit Run
// repeats on an interval, pulled out as its own method so the tick/insert logic is
// directly unit-testable without a ticker/timer or a live GRPC connection in the loop.
func (p *Poller) Tick(ctx context.Context) error {
	stats, err := p.Stats.GetMempoolStats(ctx)
	if err != nil {
		return fmt.Errorf("mempoolpoller: get mempool stats: %w", err)
	}

	snapshot := db.MempoolSnapshot{
		SnapshotTime:      p.now(),
		UnconfirmedTxs:    int32(stats.GetUnconfirmedTxs()),
		ReorgTxs:          int32(stats.GetReorgTxs()),
		UnconfirmedWeight: int64(stats.GetUnconfirmedWeight()),
	}
	if err := p.DB.InsertMempoolSnapshot(ctx, snapshot); err != nil {
		return fmt.Errorf("mempoolpoller: insert snapshot: %w", err)
	}
	return nil
}

// Run calls Tick every pollInterval until ctx is cancelled (see cmd/mempool-poller's
// use of signal.NotifyContext for SIGINT/SIGTERM-driven graceful shutdown). A single
// tick's failure is logged and retried on the next interval rather than aborting the
// whole loop - matching internal/indexer.Follow's existing "log and keep going"
// resilience, so one transient GRPC/DB hiccup doesn't kill a long-running poller
// process. Returns ctx.Err() once ctx is done.
func (p *Poller) Run(ctx context.Context, pollInterval time.Duration) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := p.Tick(ctx); err != nil {
			log.Printf("mempoolpoller: tick failed: %v (will retry)", err)
		} else {
			log.Printf("mempoolpoller: tick: inserted mempool snapshot")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
