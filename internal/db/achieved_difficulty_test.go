package db

import (
	"context"
	"testing"
)

// seedBlockWithAchievedDifficulty inserts a minimal valid `blocks` row at height with
// caller-supplied powAlgo/difficulty/achievedDifficulty (and an explicit hash, since
// cmd/backfill-achieved-difficulty's HashesForHeightRange lookup needs a real value to
// round-trip) - mirrors adjusted_difficulty_test.go's seedBlockWithAdjustedDifficulty
// shape, adding this feature's one extra field those callers don't need.
func seedBlockWithAchievedDifficulty(t *testing.T, d *DB, height uint64, hash string, powAlgo string, difficulty int64, achievedDifficulty *int64) {
	t.Helper()
	err := d.UpsertBlock(context.Background(), Block{
		Height:             height,
		Hash:               hash,
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
		AchievedDifficulty: achievedDifficulty,
	})
	if err != nil {
		t.Fatalf("db: seed block %d: %v", height, err)
	}
}

// TestUpsertBlock_AchievedDifficultyRoundTrips_Nil proves a block upserted without an
// AchievedDifficulty (nil) round-trips through GetBlock as nil - NULL in Postgres, not
// a coerced 0 - matching this column's nullable-vs-zero-meaningful convention (see
// migrations/0011_achieved_difficulty.up.sql).
func TestUpsertBlock_AchievedDifficultyRoundTrips_Nil(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	seedBlockWithAchievedDifficulty(t, d, 1500, "aa", "RXM", 1000, nil)

	got, err := d.GetBlock(ctx, 1500)
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if got.AchievedDifficulty != nil {
		t.Errorf("AchievedDifficulty = %v, want nil (not yet captured)", *got.AchievedDifficulty)
	}
}

// TestUpsertBlock_AchievedDifficultyRoundTrips_RealValue proves a block upserted with
// a real (non-nil) AchievedDifficulty round-trips through GetBlock with that exact
// value, including the case where it legitimately exceeds both Difficulty and
// AdjustedDifficulty (the real "margin cleared" scenario this feature exists to
// surface - see the Esmeralda height 961162 example in the migration's doc comment).
func TestUpsertBlock_AchievedDifficultyRoundTrips_RealValue(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	achieved := int64(193_350)
	seedBlockWithAchievedDifficulty(t, d, 1501, "bb", "RXT", 50_835, &achieved)

	got, err := d.GetBlock(ctx, 1501)
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if got.AchievedDifficulty == nil {
		t.Fatal("AchievedDifficulty = nil, want 193350")
	}
	if *got.AchievedDifficulty != 193_350 {
		t.Errorf("AchievedDifficulty = %d, want 193350", *got.AchievedDifficulty)
	}
	if got.Difficulty != 50_835 {
		t.Errorf("Difficulty = %d, want 50835 (raw must stay independent of achieved)", got.Difficulty)
	}
}

// TestUpsertBlock_AchievedDifficultyUpsertOverwrites proves re-upserting the same
// height with a different AchievedDifficulty (including going from a real value back
// to nil) actually overwrites the stored value - UpsertBlock is a full upsert, not a
// narrow column patch, so every column (including this new one) must follow
// EXCLUDED's value on conflict.
func TestUpsertBlock_AchievedDifficultyUpsertOverwrites(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	first := int64(111)
	seedBlockWithAchievedDifficulty(t, d, 1502, "cc", "C29", 1_000, &first)

	second := int64(222)
	seedBlockWithAchievedDifficulty(t, d, 1502, "cc", "C29", 1_000, &second)

	got, err := d.GetBlock(ctx, 1502)
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if got.AchievedDifficulty == nil || *got.AchievedDifficulty != 222 {
		t.Fatalf("expected AchievedDifficulty to be overwritten to 222, got %+v", got.AchievedDifficulty)
	}

	// Re-upserting with nil must clear it back to NULL, not leave the old value.
	seedBlockWithAchievedDifficulty(t, d, 1502, "cc", "C29", 1_000, nil)
	got, err = d.GetBlock(ctx, 1502)
	if err != nil {
		t.Fatalf("GetBlock (after clearing): %v", err)
	}
	if got.AchievedDifficulty != nil {
		t.Errorf("expected AchievedDifficulty to be cleared back to nil, got %v", *got.AchievedDifficulty)
	}
}

// TestSetAchievedDifficulty proves SetAchievedDifficulty updates only the
// achieved_difficulty column for the target height, leaving every other column (e.g.
// Difficulty) untouched - mirroring TestSetAdjustedDifficulty's own narrow-column
// assertion shape.
func TestSetAchievedDifficulty(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	seedBlockWithAchievedDifficulty(t, d, 1503, "dd", "SHA3X", 50_835, nil)

	achieved := int64(193_350)
	if err := d.SetAchievedDifficulty(ctx, 1503, &achieved); err != nil {
		t.Fatalf("SetAchievedDifficulty: %v", err)
	}

	got, err := d.GetBlock(ctx, 1503)
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if got.AchievedDifficulty == nil || *got.AchievedDifficulty != 193_350 {
		t.Fatalf("expected AchievedDifficulty 193350, got %+v", got.AchievedDifficulty)
	}
	if got.Difficulty != 50_835 {
		t.Errorf("expected Difficulty to remain untouched at 50835, got %d", got.Difficulty)
	}

	// Setting back to nil must clear it.
	if err := d.SetAchievedDifficulty(ctx, 1503, nil); err != nil {
		t.Fatalf("SetAchievedDifficulty (clear): %v", err)
	}
	got, err = d.GetBlock(ctx, 1503)
	if err != nil {
		t.Fatalf("GetBlock (after clear): %v", err)
	}
	if got.AchievedDifficulty != nil {
		t.Errorf("expected AchievedDifficulty to be cleared to nil, got %v", *got.AchievedDifficulty)
	}
}

// TestAchievedDifficultiesForHeightRange_DistinguishesNilFromRealValue proves the map
// AchievedDifficultiesForHeightRange returns carries a nil *int64 for a block whose
// achieved_difficulty is genuinely NULL, and a non-nil pointer to the real value for a
// block that has one - the core distinction cmd/backfill-achieved-difficulty's
// diff-check needs (see db.go's doc comment on this method).
func TestAchievedDifficultiesForHeightRange_DistinguishesNilFromRealValue(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	achieved := int64(5_000)
	seedBlockWithAchievedDifficulty(t, d, 1600, "ee", "RXM", 100, nil)
	seedBlockWithAchievedDifficulty(t, d, 1601, "ff", "RXM", 200, &achieved)

	got, err := d.AchievedDifficultiesForHeightRange(ctx, 1600, 1601)
	if err != nil {
		t.Fatalf("AchievedDifficultiesForHeightRange: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 heights in range, got %d: %+v", len(got), got)
	}
	if v, ok := got[1600]; !ok {
		t.Error("expected height 1600 to be present in the map")
	} else if v != nil {
		t.Errorf("expected height 1600's achieved difficulty to be nil (NULL), got %v", *v)
	}
	if v, ok := got[1601]; !ok {
		t.Error("expected height 1601 to be present in the map")
	} else if v == nil || *v != 5_000 {
		t.Errorf("expected height 1601's achieved difficulty to be 5000, got %+v", v)
	}
}

// TestAchievedDifficultiesForHeightRange_MissingHeightIsAbsentFromMap proves a height
// outside [fromHeight, toHeight], or never indexed at all, simply doesn't appear as a
// map key - distinct from appearing with a nil value (NULL) - matching
// AdjustedDifficultiesForHeightRange's analogous "absent key means no row" contract.
func TestAchievedDifficultiesForHeightRange_MissingHeightIsAbsentFromMap(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	seedBlockWithAchievedDifficulty(t, d, 1700, "11", "RXT", 10, nil)

	got, err := d.AchievedDifficultiesForHeightRange(ctx, 1700, 1702)
	if err != nil {
		t.Fatalf("AchievedDifficultiesForHeightRange: %v", err)
	}
	if _, ok := got[1701]; ok {
		t.Errorf("expected height 1701 (never indexed) to be absent from the map, got %+v", got)
	}
	if _, ok := got[1700]; !ok {
		t.Errorf("expected height 1700 (seeded) to be present, got %+v", got)
	}
}

// TestHashesForHeightRange proves HashesForHeightRange returns each seeded height's
// hex-encoded hash, and that a height outside the requested range (or never indexed)
// is simply absent from the map - the read cmd/backfill-achieved-difficulty depends on
// to get each block's hash straight out of Postgres rather than re-fetching the full
// block via GRPC.
func TestHashesForHeightRange(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	seedBlockWithAchievedDifficulty(t, d, 1800, "deadbeef", "RXM", 10, nil)
	seedBlockWithAchievedDifficulty(t, d, 1801, "cafebabe", "RXM", 20, nil)

	got, err := d.HashesForHeightRange(ctx, 1800, 1801)
	if err != nil {
		t.Fatalf("HashesForHeightRange: %v", err)
	}
	if got[1800] != "deadbeef" {
		t.Errorf("height 1800 hash = %q, want %q", got[1800], "deadbeef")
	}
	if got[1801] != "cafebabe" {
		t.Errorf("height 1801 hash = %q, want %q", got[1801], "cafebabe")
	}
	if _, ok := got[1802]; ok {
		t.Errorf("expected height 1802 (never indexed) to be absent from the map, got %+v", got)
	}
}
