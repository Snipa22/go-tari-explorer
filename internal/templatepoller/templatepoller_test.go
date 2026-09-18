package templatepoller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// fakeFetcher is a Fetcher backed by an in-memory canned response/error, no live GRPC
// connection required - this is the seam that makes Poller.Tick/Run unit-testable per
// this package's doc comment.
type fakeFetcher struct {
	// resp maps a requested algo to the response to return for it. errs maps a
	// requested algo to the error to return for it (checked before resp).
	resp map[tari_generated.PowAlgo_PowAlgos]*tari_generated.NewBlockTemplateResponse
	errs map[tari_generated.PowAlgo_PowAlgos]error

	// calls counts how many times GetNewBlockTemplate was invoked in total, and
	// callsByAlgo counts per-algo, so tests can assert on tick/call counts without
	// racing on wall-clock timing.
	calls       int
	callsByAlgo map[tari_generated.PowAlgo_PowAlgos]int
}

func (f *fakeFetcher) GetNewBlockTemplate(ctx context.Context, algo tari_generated.PowAlgo_PowAlgos) (*tari_generated.NewBlockTemplateResponse, error) {
	f.calls++
	if f.callsByAlgo == nil {
		f.callsByAlgo = map[tari_generated.PowAlgo_PowAlgos]int{}
	}
	f.callsByAlgo[algo]++

	if err, ok := f.errs[algo]; ok {
		return nil, err
	}
	if resp, ok := f.resp[algo]; ok {
		return resp, nil
	}
	return &tari_generated.NewBlockTemplateResponse{}, nil
}

// fakeSnapshotUpserter is a SnapshotUpserter backed by an in-memory slice, no real
// Postgres connection required - mirrors fakeFetcher's rationale, and lets this
// package's tests assert on exactly which snapshots were upserted (and how many times)
// without a database at all.
type fakeSnapshotUpserter struct {
	// seen tracks which (algo, height) pairs have already been "inserted", so
	// repeated upserts of the same pair report inserted=false - a minimal in-memory
	// stand-in for the real ON CONFLICT (algo, height) DO NOTHING behavior.
	seen map[[2]any]bool
	rows []db.TemplateDifficultySnapshot

	err error
	// calls counts how many times UpsertTemplateDifficultySnapshot was invoked.
	calls int
}

func (f *fakeSnapshotUpserter) UpsertTemplateDifficultySnapshot(ctx context.Context, s db.TemplateDifficultySnapshot) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	if f.seen == nil {
		f.seen = map[[2]any]bool{}
	}
	key := [2]any{s.Algo, s.Height}
	if f.seen[key] {
		return false, nil
	}
	f.seen[key] = true
	f.rows = append(f.rows, s)
	return true, nil
}

// TestTick_HappyPathAllFourAlgos proves one Tick call fetches all 4 pow-algos' block
// templates and upserts one template_difficulty_snapshots row per algo, with the
// fetched height/target_difficulty/reward and a deterministic (fake-clock) RecordedAt
// carried through correctly.
func TestTick_HappyPathAllFourAlgos(t *testing.T) {
	fixedTime := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	template := func(height, targetDifficulty, reward uint64) *tari_generated.NewBlockTemplateResponse {
		return &tari_generated.NewBlockTemplateResponse{
			NewBlockTemplate: &tari_generated.NewBlockTemplate{
				Header: &tari_generated.NewBlockHeaderTemplate{Height: height},
			},
			MinerData: &tari_generated.MinerData{TargetDifficulty: targetDifficulty, Reward: reward},
		}
	}

	fetcher := &fakeFetcher{
		resp: map[tari_generated.PowAlgo_PowAlgos]*tari_generated.NewBlockTemplateResponse{
			tari_generated.PowAlgo_POW_ALGOS_RANDOMXM: template(1000, 111, 5000),
			tari_generated.PowAlgo_POW_ALGOS_SHA3X:    template(2000, 222, 5001),
			tari_generated.PowAlgo_POW_ALGOS_RANDOMXT: template(3000, 333, 5002),
			tari_generated.PowAlgo_POW_ALGOS_CUCKAROO: template(4000, 444, 5003),
		},
	}
	upserter := &fakeSnapshotUpserter{}

	poller := New(fetcher, upserter)
	poller.Now = func() time.Time { return fixedTime }

	n, err := poller.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 4 {
		t.Fatalf("expected 4 rows inserted across the 4 algos, got %d", n)
	}
	if fetcher.calls != 4 {
		t.Fatalf("expected exactly 4 fetch calls (one per algo), got %d", fetcher.calls)
	}
	if len(upserter.rows) != 4 {
		t.Fatalf("expected exactly 4 upserted rows, got %d: %+v", len(upserter.rows), upserter.rows)
	}

	byAlgo := map[string]db.TemplateDifficultySnapshot{}
	for _, r := range upserter.rows {
		byAlgo[r.Algo] = r
	}
	if rxm := byAlgo["RXM"]; rxm.Height != 1000 || rxm.TargetDifficulty != 111 || rxm.Reward != 5000 {
		t.Errorf("unexpected RXM snapshot: %+v", rxm)
	}
	if sha := byAlgo["SHA3X"]; sha.Height != 2000 || sha.TargetDifficulty != 222 || sha.Reward != 5001 {
		t.Errorf("unexpected SHA3X snapshot: %+v", sha)
	}
	if rxt := byAlgo["RXT"]; rxt.Height != 3000 || rxt.TargetDifficulty != 333 || rxt.Reward != 5002 {
		t.Errorf("unexpected RXT snapshot: %+v", rxt)
	}
	if c29 := byAlgo["C29"]; c29.Height != 4000 || c29.TargetDifficulty != 444 || c29.Reward != 5003 {
		t.Errorf("unexpected C29 snapshot: %+v", c29)
	}
	for algo, r := range byAlgo {
		if !r.RecordedAt.Equal(fixedTime) {
			t.Errorf("expected RecordedAt %v for algo %s, got %v", fixedTime, algo, r.RecordedAt)
		}
	}
}

// TestTick_OneAlgoFetchErrorDoesNotBlockOthers proves that when one algo's
// GetNewBlockTemplate call fails, the other 3 algos are still fetched and upserted in
// the same tick, and Tick returns the first error encountered rather than swallowing
// it, matching difficultypoller.Poller.Tick's "log and continue" aggregation style.
func TestTick_OneAlgoFetchErrorDoesNotBlockOthers(t *testing.T) {
	template := func(height, targetDifficulty, reward uint64) *tari_generated.NewBlockTemplateResponse {
		return &tari_generated.NewBlockTemplateResponse{
			NewBlockTemplate: &tari_generated.NewBlockTemplate{
				Header: &tari_generated.NewBlockHeaderTemplate{Height: height},
			},
			MinerData: &tari_generated.MinerData{TargetDifficulty: targetDifficulty, Reward: reward},
		}
	}

	fetcher := &fakeFetcher{
		resp: map[tari_generated.PowAlgo_PowAlgos]*tari_generated.NewBlockTemplateResponse{
			tari_generated.PowAlgo_POW_ALGOS_SHA3X:    template(2000, 222, 5001),
			tari_generated.PowAlgo_POW_ALGOS_RANDOMXT: template(3000, 333, 5002),
			tari_generated.PowAlgo_POW_ALGOS_CUCKAROO: template(4000, 444, 5003),
		},
		errs: map[tari_generated.PowAlgo_PowAlgos]error{
			tari_generated.PowAlgo_POW_ALGOS_RANDOMXM: errors.New("boom: RXM node unreachable"),
		},
	}
	upserter := &fakeSnapshotUpserter{}

	poller := New(fetcher, upserter)

	n, err := poller.Tick(context.Background())
	if err == nil {
		t.Fatal("expected Tick to return the RXM fetch error")
	}
	if n != 3 {
		t.Fatalf("expected the other 3 algos to still be inserted despite RXM's fetch error, got %d", n)
	}
	if fetcher.calls != 4 {
		t.Fatalf("expected all 4 algos to be attempted (RXM's failure shouldn't skip the rest), got %d calls", fetcher.calls)
	}
	if len(upserter.rows) != 3 {
		t.Fatalf("expected exactly 3 upserted rows (RXM excluded), got %d: %+v", len(upserter.rows), upserter.rows)
	}
	for _, r := range upserter.rows {
		if r.Algo == "RXM" {
			t.Errorf("did not expect an RXM row to be upserted after its fetch failed: %+v", r)
		}
	}
}

// TestTick_AlreadyKnownHeightStillCallsUpsert proves the poller does NOT do its own
// height-change detection - it calls UpsertTemplateDifficultySnapshot every tick for
// every algo it successfully fetches, even when the template height hasn't changed
// since the previous tick. Idempotency (no duplicate row, no error) is entirely the
// DB layer's job via ON CONFLICT DO NOTHING, exercised here through
// fakeSnapshotUpserter's own dedup bookkeeping rather than a real Postgres connection.
func TestTick_AlreadyKnownHeightStillCallsUpsert(t *testing.T) {
	template := &tari_generated.NewBlockTemplateResponse{
		NewBlockTemplate: &tari_generated.NewBlockTemplate{
			Header: &tari_generated.NewBlockHeaderTemplate{Height: 1000},
		},
		MinerData: &tari_generated.MinerData{TargetDifficulty: 111, Reward: 5000},
	}
	fetcher := &fakeFetcher{
		resp: map[tari_generated.PowAlgo_PowAlgos]*tari_generated.NewBlockTemplateResponse{
			tari_generated.PowAlgo_POW_ALGOS_RANDOMXM: template,
			tari_generated.PowAlgo_POW_ALGOS_SHA3X:    template,
			tari_generated.PowAlgo_POW_ALGOS_RANDOMXT: template,
			tari_generated.PowAlgo_POW_ALGOS_CUCKAROO: template,
		},
	}
	upserter := &fakeSnapshotUpserter{}
	poller := New(fetcher, upserter)

	firstN, err := poller.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick (1st): %v", err)
	}
	if firstN != 4 {
		t.Fatalf("expected 4 rows inserted on first observation of 4 algos, got %d", firstN)
	}

	secondN, err := poller.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick (2nd): %v", err)
	}
	if secondN != 0 {
		t.Fatalf("expected 0 NEW rows on the second tick (unchanged height), got %d", secondN)
	}

	// The poller itself must still have called Upsert once per algo on both ticks -
	// it's the fake DB's dedup, not the poller, that turned the 2nd tick's inserts
	// into no-ops.
	if fetcher.calls != 8 {
		t.Fatalf("expected 8 total fetch calls across 2 ticks (4 algos x 2), got %d", fetcher.calls)
	}
	if upserter.calls != 8 {
		t.Fatalf("expected the poller to call Upsert 8 times across 2 ticks (4 algos x 2), not skip the 2nd tick's calls, got %d", upserter.calls)
	}
	if len(upserter.rows) != 4 {
		t.Fatalf("expected exactly 4 total rows ever inserted (no duplicates), got %d", len(upserter.rows))
	}
}

// TestTick_UpsertErrorDoesNotBlockOtherAlgos proves that when one algo's
// UpsertTemplateDifficultySnapshot call fails, the other algos are still attempted in
// the same tick, and the first upsert error is returned.
func TestTick_UpsertErrorDoesNotBlockOtherAlgos(t *testing.T) {
	template := &tari_generated.NewBlockTemplateResponse{
		NewBlockTemplate: &tari_generated.NewBlockTemplate{
			Header: &tari_generated.NewBlockHeaderTemplate{Height: 1000},
		},
		MinerData: &tari_generated.MinerData{TargetDifficulty: 111, Reward: 5000},
	}
	fetcher := &fakeFetcher{
		resp: map[tari_generated.PowAlgo_PowAlgos]*tari_generated.NewBlockTemplateResponse{
			tari_generated.PowAlgo_POW_ALGOS_RANDOMXM: template,
			tari_generated.PowAlgo_POW_ALGOS_SHA3X:    template,
			tari_generated.PowAlgo_POW_ALGOS_RANDOMXT: template,
			tari_generated.PowAlgo_POW_ALGOS_CUCKAROO: template,
		},
	}
	upserter := &fakeSnapshotUpserter{err: errors.New("boom: db unreachable")}
	poller := New(fetcher, upserter)

	n, err := poller.Tick(context.Background())
	if err == nil {
		t.Fatal("expected Tick to propagate the upsert error")
	}
	if n != 0 {
		t.Fatalf("expected 0 rows inserted when every upsert fails, got %d", n)
	}
	if upserter.calls != 4 {
		t.Fatalf("expected all 4 algos' upserts to still be attempted despite errors, got %d calls", upserter.calls)
	}
}

// TestRun_TicksUntilContextCancelled proves Run keeps calling Tick on the configured
// interval until its context is cancelled, then returns promptly with ctx.Err() - the
// same graceful-shutdown contract internal/difficultypoller.Poller.Run/
// internal/mempoolpoller.Poller.Run already provide.
func TestRun_TicksUntilContextCancelled(t *testing.T) {
	fetcher := &fakeFetcher{}
	upserter := &fakeSnapshotUpserter{}
	poller := New(fetcher, upserter)

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
}
