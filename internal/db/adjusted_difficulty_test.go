package db

import (
	"context"
	"testing"
	"time"
)

// seedBlockWithAdjustedDifficulty inserts a minimal valid `blocks` row at height with
// caller-supplied powAlgo/difficulty/adjustedDifficulty - used by this file's tests to
// exercise the new (migrations/0010_adjusted_difficulty.up.sql) nullable
// adjusted_difficulty column, mirroring db_test.go's seedBlockFull shape but adding
// the one extra field those callers don't need.
func seedBlockWithAdjustedDifficulty(t *testing.T, d *DB, height uint64, powAlgo string, difficulty int64, adjustedDifficulty *int64) {
	t.Helper()
	err := d.UpsertBlock(context.Background(), Block{
		Height:             height,
		Hash:               "aa",
		PrevHash:           "bb",
		OutputMr:           []byte{},
		BlockOutputMr:      []byte{},
		KernelMr:           []byte{},
		InputMr:            []byte{},
		TotalKernelOffset:  []byte{},
		TotalScriptOffset:  []byte{},
		ValidatorNodeMr:    []byte{},
		PowData:            []byte{},
		PowAlgo:            powAlgo,
		Difficulty:         difficulty,
		AdjustedDifficulty: adjustedDifficulty,
	})
	if err != nil {
		t.Fatalf("db: seed block %d: %v", height, err)
	}
}

// TestUpsertBlock_AdjustedDifficultyRoundTrips_Nil proves a block upserted without an
// AdjustedDifficulty (nil) round-trips through GetBlock as nil - NULL in Postgres,
// not a coerced 0 - matching this column's nullable-vs-zero-meaningful convention
// (see migrations/0010_adjusted_difficulty.up.sql).
func TestUpsertBlock_AdjustedDifficultyRoundTrips_Nil(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	seedBlockWithAdjustedDifficulty(t, d, 500, "RXM", 1000, nil)

	got, err := d.GetBlock(ctx, 500)
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if got.AdjustedDifficulty != nil {
		t.Errorf("AdjustedDifficulty = %v, want nil (not yet captured)", *got.AdjustedDifficulty)
	}
}

// TestUpsertBlock_AdjustedDifficultyRoundTrips_RealValue proves a block upserted with
// a real (non-nil) AdjustedDifficulty round-trips through GetBlock with that exact
// value, including the case where it legitimately differs from the raw Difficulty
// (the TIP-004 backoff-adjusted scenario this feature exists for).
func TestUpsertBlock_AdjustedDifficultyRoundTrips_RealValue(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	adjusted := int64(32_000)
	seedBlockWithAdjustedDifficulty(t, d, 501, "RXT", 1_000, &adjusted)

	got, err := d.GetBlock(ctx, 501)
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if got.AdjustedDifficulty == nil {
		t.Fatal("AdjustedDifficulty = nil, want 32000")
	}
	if *got.AdjustedDifficulty != 32_000 {
		t.Errorf("AdjustedDifficulty = %d, want 32000", *got.AdjustedDifficulty)
	}
	if got.Difficulty != 1_000 {
		t.Errorf("Difficulty = %d, want 1000 (raw must stay independent of adjusted)", got.Difficulty)
	}
}

// TestUpsertBlock_AdjustedDifficultyUpsertOverwrites proves re-upserting the same
// height with a different AdjustedDifficulty (including going from a real value back
// to nil) actually overwrites the stored value - UpsertBlock is a full upsert, not a
// narrow column patch, so every column (including this new one) must follow
// EXCLUDED's value on conflict.
func TestUpsertBlock_AdjustedDifficultyUpsertOverwrites(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	first := int64(111)
	seedBlockWithAdjustedDifficulty(t, d, 502, "C29", 1_000, &first)

	second := int64(222)
	seedBlockWithAdjustedDifficulty(t, d, 502, "C29", 1_000, &second)

	got, err := d.GetBlock(ctx, 502)
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if got.AdjustedDifficulty == nil || *got.AdjustedDifficulty != 222 {
		t.Fatalf("expected AdjustedDifficulty to be overwritten to 222, got %+v", got.AdjustedDifficulty)
	}

	// Re-upserting with nil must clear it back to NULL, not leave the old value.
	seedBlockWithAdjustedDifficulty(t, d, 502, "C29", 1_000, nil)
	got, err = d.GetBlock(ctx, 502)
	if err != nil {
		t.Fatalf("GetBlock (after clearing): %v", err)
	}
	if got.AdjustedDifficulty != nil {
		t.Errorf("expected AdjustedDifficulty to be cleared back to nil, got %v", *got.AdjustedDifficulty)
	}
}

// TestSetAdjustedDifficulty proves SetAdjustedDifficulty updates only the
// adjusted_difficulty column for the target height, leaving every other column (e.g.
// Difficulty) untouched - mirroring TestSetRewardMicroMinotari's own narrow-column
// assertion shape.
func TestSetAdjustedDifficulty(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	seedBlockWithAdjustedDifficulty(t, d, 503, "SHA3X", 9_999, nil)

	adjusted := int64(319_968) // 9999 * 32, a plausible TIP-004 32x backoff value
	if err := d.SetAdjustedDifficulty(ctx, 503, &adjusted); err != nil {
		t.Fatalf("SetAdjustedDifficulty: %v", err)
	}

	got, err := d.GetBlock(ctx, 503)
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if got.AdjustedDifficulty == nil || *got.AdjustedDifficulty != 319_968 {
		t.Fatalf("expected AdjustedDifficulty 319968, got %+v", got.AdjustedDifficulty)
	}
	if got.Difficulty != 9_999 {
		t.Errorf("expected Difficulty to remain untouched at 9999, got %d", got.Difficulty)
	}

	// Setting back to nil must clear it.
	if err := d.SetAdjustedDifficulty(ctx, 503, nil); err != nil {
		t.Fatalf("SetAdjustedDifficulty (clear): %v", err)
	}
	got, err = d.GetBlock(ctx, 503)
	if err != nil {
		t.Fatalf("GetBlock (after clear): %v", err)
	}
	if got.AdjustedDifficulty != nil {
		t.Errorf("expected AdjustedDifficulty to be cleared to nil, got %v", *got.AdjustedDifficulty)
	}
}

// TestAdjustedDifficultiesForHeightRange_DistinguishesNilFromRealValue proves the map
// AdjustedDifficultiesForHeightRange returns carries a nil *int64 for a block whose
// adjusted_difficulty is genuinely NULL, and a non-nil pointer to the real value for
// a block that has one - the core distinction cmd/backfill-adjusted-difficulty's
// diff-check needs (see db.go's doc comment on this method).
func TestAdjustedDifficultiesForHeightRange_DistinguishesNilFromRealValue(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	adjusted := int64(5_000)
	seedBlockWithAdjustedDifficulty(t, d, 600, "RXM", 100, nil)
	seedBlockWithAdjustedDifficulty(t, d, 601, "RXM", 200, &adjusted)

	got, err := d.AdjustedDifficultiesForHeightRange(ctx, 600, 601)
	if err != nil {
		t.Fatalf("AdjustedDifficultiesForHeightRange: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 heights in range, got %d: %+v", len(got), got)
	}
	if v, ok := got[600]; !ok {
		t.Error("expected height 600 to be present in the map")
	} else if v != nil {
		t.Errorf("expected height 600's adjusted difficulty to be nil (NULL), got %v", *v)
	}
	if v, ok := got[601]; !ok {
		t.Error("expected height 601 to be present in the map")
	} else if v == nil || *v != 5_000 {
		t.Errorf("expected height 601's adjusted difficulty to be 5000, got %+v", v)
	}
}

// TestAdjustedDifficultiesForHeightRange_MissingHeightIsAbsentFromMap proves a height
// outside [fromHeight, toHeight], or never indexed at all, simply doesn't appear as a
// map key - distinct from appearing with a nil value (NULL) - matching
// RewardsForHeightRange's analogous "absent key means no row" contract.
func TestAdjustedDifficultiesForHeightRange_MissingHeightIsAbsentFromMap(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	seedBlockWithAdjustedDifficulty(t, d, 700, "RXT", 10, nil)

	got, err := d.AdjustedDifficultiesForHeightRange(ctx, 700, 702)
	if err != nil {
		t.Fatalf("AdjustedDifficultiesForHeightRange: %v", err)
	}
	if _, ok := got[701]; ok {
		t.Errorf("expected height 701 (never indexed) to be absent from the map, got %+v", got)
	}
	if _, ok := got[700]; !ok {
		t.Errorf("expected height 700 (seeded) to be present, got %+v", got)
	}
}

// TestCurrentDifficultyPerAlgo_CarriesAdjustedDifficulty proves
// CurrentDifficultyPerAlgo's result now also carries each algo's latest block's
// AdjustedDifficulty (nil-safe for an algo whose latest block hasn't captured it
// yet) - the read internal/difficultypoller.Poller.Tick depends on to carry this
// value through into difficulty_snapshots.
func TestCurrentDifficultyPerAlgo_CarriesAdjustedDifficulty(t *testing.T) {
	d := openDifficultyTestDB(t)
	ctx := context.Background()

	adjusted := int64(640_000)
	seedBlockWithAdjustedDifficulty(t, d, 800, "RXM", 20_000, &adjusted)
	seedBlockWithAdjustedDifficulty(t, d, 801, "SHA3X", 5_000, nil)

	got, err := d.CurrentDifficultyPerAlgo(ctx)
	if err != nil {
		t.Fatalf("CurrentDifficultyPerAlgo: %v", err)
	}
	byAlgo := map[string]CurrentDifficultyRow{}
	for _, r := range got {
		byAlgo[r.Algo] = r
	}
	rxm, ok := byAlgo["RXM"]
	if !ok {
		t.Fatal("expected RXM to be present")
	}
	if rxm.AdjustedDifficulty == nil || *rxm.AdjustedDifficulty != 640_000 {
		t.Errorf("expected RXM AdjustedDifficulty 640000, got %+v", rxm.AdjustedDifficulty)
	}
	sha, ok := byAlgo["SHA3X"]
	if !ok {
		t.Fatal("expected SHA3X to be present")
	}
	if sha.AdjustedDifficulty != nil {
		t.Errorf("expected SHA3X AdjustedDifficulty to be nil (not yet captured), got %v", *sha.AdjustedDifficulty)
	}
}

// TestUpsertDifficultySnapshot_CarriesAdjustedDifficulty proves a DifficultySnapshot
// upserted with a non-nil AdjustedDifficulty round-trips through
// LatestDifficultySnapshots with that exact value, and that a snapshot upserted with
// nil round-trips as nil (not a coerced 0) - matching blocks.adjusted_difficulty's own
// nullable convention one layer up.
func TestUpsertDifficultySnapshot_CarriesAdjustedDifficulty(t *testing.T) {
	d := openDifficultyTestDB(t)
	ctx := context.Background()

	recordedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	adjusted := int64(77_000)

	if _, err := d.UpsertDifficultySnapshot(ctx, DifficultySnapshot{
		Algo:               "RXM",
		Height:             900,
		Difficulty:         2_000,
		AdjustedDifficulty: &adjusted,
		RecordedAt:         recordedAt,
	}); err != nil {
		t.Fatalf("UpsertDifficultySnapshot (with adjusted): %v", err)
	}
	if _, err := d.UpsertDifficultySnapshot(ctx, DifficultySnapshot{
		Algo:       "SHA3X",
		Height:     901,
		Difficulty: 3_000,
		RecordedAt: recordedAt,
	}); err != nil {
		t.Fatalf("UpsertDifficultySnapshot (without adjusted): %v", err)
	}

	got, err := d.LatestDifficultySnapshots(ctx)
	if err != nil {
		t.Fatalf("LatestDifficultySnapshots: %v", err)
	}
	byAlgo := map[string]DifficultySnapshot{}
	for _, r := range got {
		byAlgo[r.Algo] = r
	}
	rxm, ok := byAlgo["RXM"]
	if !ok {
		t.Fatal("expected RXM snapshot to be present")
	}
	if rxm.AdjustedDifficulty == nil || *rxm.AdjustedDifficulty != 77_000 {
		t.Errorf("expected RXM AdjustedDifficulty 77000, got %+v", rxm.AdjustedDifficulty)
	}
	sha, ok := byAlgo["SHA3X"]
	if !ok {
		t.Fatal("expected SHA3X snapshot to be present")
	}
	if sha.AdjustedDifficulty != nil {
		t.Errorf("expected SHA3X AdjustedDifficulty to be nil, got %v", *sha.AdjustedDifficulty)
	}
}
