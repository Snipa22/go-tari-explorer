// Package difficultypoller implements the tick-and-upsert loop behind
// cmd/difficulty-poller: on a short configurable interval, read each pow-algo's latest
// indexed (height, difficulty) from the already-indexed `blocks` table (via
// internal/db.DB.CurrentDifficultyPerAlgo - no live base-node GRPC call, see that
// method's doc comment for why the difficulty on an algo's most-recently-indexed block
// already is that algo's live current target) and upsert a new difficulty_snapshots
// row for any algo whose height has actually advanced since the last recorded snapshot.
//
// Unlike internal/mempoolpoller (which inserts exactly one new row every tick, because
// a mempool snapshot has no natural key to de-duplicate against), this poller ticks
// far more often than blocks are actually found per algo - so most ticks are expected
// to observe no change and insert nothing. That's the point: Postgres's
// ON CONFLICT (algo, height) DO NOTHING (db.UpsertDifficultySnapshot) makes re-checking
// an unchanged height on every tick cheap and idempotent, so difficulty_snapshots ends
// up with exactly one row per (algo, height) ever actually observed, not one per tick.
package difficultypoller

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// Fetcher is the subset of *internal/db.DB's methods Poller needs to read the current
// per-algo (height, difficulty) state. Satisfied by *db.DB (via
// CurrentDifficultyPerAlgo); declared here so tests can substitute a fake instead of a
// real Postgres connection for the fetch side while still exercising the real upsert
// side against a real database (see difficultypoller_test.go).
type Fetcher interface {
	CurrentDifficultyPerAlgo(ctx context.Context) ([]db.CurrentDifficultyRow, error)
}

// SnapshotUpserter is the subset of *internal/db.DB's methods Poller needs to persist
// a new (algo, height) observation. Satisfied by *db.DB.
type SnapshotUpserter interface {
	UpsertDifficultySnapshot(ctx context.Context, s db.DifficultySnapshot) (inserted bool, err error)
}

// compile-time assertions that *db.DB satisfies both seams (they exist for
// testability, per this package's doc comment, but production code always wires up
// the real thing for both).
var (
	_ Fetcher          = (*db.DB)(nil)
	_ SnapshotUpserter = (*db.DB)(nil)
)

// Poller bundles the fetch and upsert dependencies Tick/Run need.
type Poller struct {
	Source Fetcher
	Sink   SnapshotUpserter

	// Now returns the current time, used as each newly-inserted snapshot's
	// RecordedAt. Defaults to time.Now().UTC() (via the now() helper below) when
	// left nil, as New leaves it - exists purely so tests can assert against a
	// known, deterministic RecordedAt instead of a moving target.
	Now func() time.Time
}

// New constructs a Poller reading from source and writing to sink. Now is left nil
// (defaults to time.Now via the now() helper); set p.Now directly after construction
// if a test needs a fixed clock.
func New(source Fetcher, sink SnapshotUpserter) *Poller {
	return &Poller{Source: source, Sink: sink}
}

// now returns p.Now() if set, else time.Now().UTC().
func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

// Tick fetches every algo's current (height, difficulty) from p.Source and attempts to
// upsert a difficulty_snapshots row for each one, via p.Sink (ON CONFLICT (algo,
// height) DO NOTHING - see db.UpsertDifficultySnapshot). Returns the number of rows
// actually inserted (0 on a tick where nothing changed - the common case between
// blocks) and the first upsert error encountered, if any (a single algo's upsert
// failing doesn't stop the others in the same tick from being attempted).
func (p *Poller) Tick(ctx context.Context) (inserted int, err error) {
	rows, ferr := p.Source.CurrentDifficultyPerAlgo(ctx)
	if ferr != nil {
		return 0, fmt.Errorf("difficultypoller: fetch current difficulty per algo: %w", ferr)
	}

	now := p.now()
	var firstErr error
	for _, r := range rows {
		ok, uerr := p.Sink.UpsertDifficultySnapshot(ctx, db.DifficultySnapshot{
			Algo:       r.Algo,
			Height:     r.Height,
			Difficulty: r.Difficulty,
			RecordedAt: now,
		})
		if uerr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("difficultypoller: upsert snapshot for algo %s height %d: %w", r.Algo, r.Height, uerr)
			}
			continue
		}
		if ok {
			inserted++
		}
	}
	return inserted, firstErr
}

// Run calls Tick every pollInterval until ctx is cancelled, matching
// internal/mempoolpoller.Poller.Run's graceful-shutdown contract (via
// signal.NotifyContext in cmd/difficulty-poller). A single tick's failure is logged and
// retried on the next interval rather than aborting the whole loop. Returns ctx.Err()
// once ctx is done.
func (p *Poller) Run(ctx context.Context, pollInterval time.Duration) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		if n, err := p.Tick(ctx); err != nil {
			log.Printf("difficultypoller: tick failed: %v (will retry)", err)
		} else if n > 0 {
			log.Printf("difficultypoller: tick: inserted %d new difficulty snapshot(s)", n)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
