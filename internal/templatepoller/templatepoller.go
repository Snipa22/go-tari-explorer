// Package templatepoller implements the tick-and-upsert loop behind
// cmd/template-difficulty-poller: on a configurable interval, fetch the LIVE base
// node's current block template for each of the 4 pow-algos (GetNewBlockTemplate, via
// internal/nodeclient - a real GRPC call, not a read of this repo's already-indexed
// `blocks` table) and upsert a new template_difficulty_snapshots row for each one.
//
// This is the forward-looking counterpart to internal/difficultypoller:
// difficultypoller reads the difficulty already stamped on an algo's most-recently
// INDEXED block (a block that was already mined); this package reads the target
// difficulty for the NEXT block an algo hasn't mined yet, straight off the live
// daemon's block-template RPC. Because of that, this package's shape mirrors
// internal/mempoolpoller (which also polls a live GRPC source every tick) rather than
// difficultypoller's DB-only shape: Poller needs both a live-node Fetcher and a
// SnapshotUpserter, expressed as small interfaces so tests can substitute fakes for
// both instead of requiring a live GRPC connection or a real Postgres database (see
// templatepoller_test.go).
//
// Unlike difficultypoller.Poller.Tick (which only upserts algos whose fetched height
// actually appears in its Source's result set), this Tick always calls
// UpsertTemplateDifficultySnapshot for every algo it successfully fetches a template
// for, every tick - deliberately not tracking per-algo "have we seen this height
// already" state itself. Idempotency is entirely the DB layer's job (ON CONFLICT
// (algo, height) DO NOTHING - see migrations/0009_template_difficulty_snapshots.up.sql
// and db.UpsertTemplateDifficultySnapshot), so re-upserting an unchanged height on
// every 1s tick is cheap and correct rather than a bug to optimize away here.
package templatepoller

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"

	"github.com/Snipa22/go-tari-explorer/internal/db"
	"github.com/Snipa22/go-tari-explorer/internal/nodeclient"
	"github.com/Snipa22/go-tari-explorer/internal/poolattr"
)

// Fetcher is the subset of *internal/nodeclient.Client's methods Poller needs.
// Satisfied by *nodeclient.Client; declared here so tests can substitute a fake
// fetcher instead of dialing a real base node.
type Fetcher interface {
	GetNewBlockTemplate(ctx context.Context, algo tari_generated.PowAlgo_PowAlgos) (*tari_generated.NewBlockTemplateResponse, error)
}

// SnapshotUpserter is the subset of *internal/db.DB's methods Poller needs. Satisfied
// by *db.DB; declared here for the same fake-substitution reason as Fetcher.
type SnapshotUpserter interface {
	UpsertTemplateDifficultySnapshot(ctx context.Context, s db.TemplateDifficultySnapshot) (inserted bool, err error)
}

// compile-time assertions that the real types satisfy both seams (they exist for
// testability, per this package's doc comment, but production code always wires up
// the real thing for both).
var (
	_ Fetcher          = (*nodeclient.Client)(nil)
	_ SnapshotUpserter = (*db.DB)(nil)
)

// algoMapping pairs one of the 4 protocol pow-algo enum values with this repo's own
// poolattr.PowAlgo string constant, so Tick has a single fixed list to iterate rather
// than duplicating the enum<->string mapping inline. Order matches the brief's/
// tari_generated.PowAlgo_PowAlgos' own declaration order (RANDOMXM, SHA3X, RANDOMXT,
// CUCKAROO), not alphabetical.
type algoMapping struct {
	RPCAlgo tari_generated.PowAlgo_PowAlgos
	Algo    poolattr.PowAlgo
}

var algoMappings = []algoMapping{
	{tari_generated.PowAlgo_POW_ALGOS_RANDOMXM, poolattr.PowAlgoRXM},
	{tari_generated.PowAlgo_POW_ALGOS_SHA3X, poolattr.PowAlgoSHA3X},
	{tari_generated.PowAlgo_POW_ALGOS_RANDOMXT, poolattr.PowAlgoRXT},
	{tari_generated.PowAlgo_POW_ALGOS_CUCKAROO, poolattr.PowAlgoC29},
}

// Poller bundles the live-node and DB dependencies Tick/Run need.
type Poller struct {
	Fetcher Fetcher
	DB      SnapshotUpserter

	// Now returns the current time, used as each newly-inserted snapshot's
	// RecordedAt. Defaults to time.Now().UTC() (via the now() helper below) when
	// left nil, as New leaves it - exists purely so tests can assert against a
	// known, deterministic RecordedAt instead of a moving target.
	Now func() time.Time
}

// New constructs a Poller fetching from fetcher and writing to database. Now is left
// nil (defaults to time.Now via the now() helper); set p.Now directly after
// construction if a test needs a fixed clock.
func New(fetcher Fetcher, database SnapshotUpserter) *Poller {
	return &Poller{Fetcher: fetcher, DB: database}
}

// now returns p.Now() if set, else time.Now().UTC().
func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

// Tick fetches the current block template for each of the 4 pow-algos from p.Fetcher
// and upserts a template_difficulty_snapshots row for each one via p.DB (ON CONFLICT
// (algo, height) DO NOTHING - see db.UpsertTemplateDifficultySnapshot). Returns the
// number of rows actually inserted (0 on a tick where no algo's template height
// changed, the common case between blocks) and the first fetch/upsert error
// encountered, if any - matching difficultypoller.Poller.Tick's exact aggregation
// style: a single algo's fetch or upsert failure is recorded but does NOT stop the
// remaining algos in the same tick from being attempted.
func (p *Poller) Tick(ctx context.Context) (inserted int, err error) {
	now := p.now()
	var firstErr error

	for _, m := range algoMappings {
		resp, ferr := p.Fetcher.GetNewBlockTemplate(ctx, m.RPCAlgo)
		if ferr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("templatepoller: get new block template for algo %s: %w", m.Algo, ferr)
			}
			continue
		}

		snapshot := db.TemplateDifficultySnapshot{
			Algo:             string(m.Algo),
			Height:           int64(resp.GetNewBlockTemplate().GetHeader().GetHeight()),
			TargetDifficulty: int64(resp.GetMinerData().GetTargetDifficulty()),
			Reward:           int64(resp.GetMinerData().GetReward()),
			RecordedAt:       now,
		}
		ok, uerr := p.DB.UpsertTemplateDifficultySnapshot(ctx, snapshot)
		if uerr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("templatepoller: upsert snapshot for algo %s height %d: %w", m.Algo, snapshot.Height, uerr)
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
// internal/difficultypoller.Poller.Run's/internal/mempoolpoller.Poller.Run's identical
// graceful-shutdown contract (via signal.NotifyContext in
// cmd/template-difficulty-poller). A single tick's failure is logged and retried on
// the next interval rather than aborting the whole loop. Returns ctx.Err() once ctx is
// done.
func (p *Poller) Run(ctx context.Context, pollInterval time.Duration) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		if n, err := p.Tick(ctx); err != nil {
			log.Printf("templatepoller: tick failed: %v (will retry)", err)
		} else if n > 0 {
			log.Printf("templatepoller: tick: inserted %d new template difficulty snapshot(s)", n)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
