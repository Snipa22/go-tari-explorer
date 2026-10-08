package server

import (
	"reflect"
	"testing"

	"github.com/Snipa22/go-tari-explorer/internal/analysis"
	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// wantAlgoOrder asserts got has exactly one row per analysis.AlgoOrder entry, in that
// fixed order, regardless of what order the newAlgoGlanceRows inputs arrived in.
func wantAlgoOrder(t *testing.T, got []algoGlanceRow) {
	t.Helper()
	if len(got) != len(analysis.AlgoOrder) {
		t.Fatalf("len(got) = %d, want %d (len(analysis.AlgoOrder))", len(got), len(analysis.AlgoOrder))
	}
	for i, algo := range analysis.AlgoOrder {
		if got[i].Algo != algo {
			t.Errorf("got[%d].Algo = %q, want %q (analysis.AlgoOrder order)", i, got[i].Algo, algo)
		}
	}
}

func TestNewAlgoGlanceRows_AlwaysReturnsAllAlgosInFixedOrder(t *testing.T) {
	got := newAlgoGlanceRows(nil, nil, nil)
	wantAlgoOrder(t, got)
}

func TestNewAlgoGlanceRows_AlgoPresentInBothInputs_UsesRealValues(t *testing.T) {
	algos := []db.AlgoCountRow{
		{Algo: "RXM", Count: 42, AvgDifficulty: 1234.5},
	}
	snapshots := []db.TemplateDifficultySnapshot{
		{Algo: "RXM", TargetDifficulty: 9876, Height: 100},
	}

	got := newAlgoGlanceRows(algos, snapshots, nil)
	wantAlgoOrder(t, got)

	row := got[0] // RXM is analysis.AlgoOrder[0]
	if row.Count != 42 {
		t.Errorf("Count = %d, want 42", row.Count)
	}
	if row.AvgDifficultyDisplay != "1,234.50" {
		t.Errorf("AvgDifficultyDisplay = %q, want %q", row.AvgDifficultyDisplay, "1,234.50")
	}
	if row.CurrentDifficultyDisplay != "9,876" {
		t.Errorf("CurrentDifficultyDisplay = %q, want %q", row.CurrentDifficultyDisplay, "9,876")
	}
}

func TestNewAlgoGlanceRows_AlgoPresentInNeitherInput_DefaultsGracefully(t *testing.T) {
	// RXT present in neither input.
	algos := []db.AlgoCountRow{
		{Algo: "RXM", Count: 1, AvgDifficulty: 1},
	}
	snapshots := []db.TemplateDifficultySnapshot{
		{Algo: "RXM", TargetDifficulty: 1},
	}

	got := newAlgoGlanceRows(algos, snapshots, nil)
	wantAlgoOrder(t, got)

	var rxt algoGlanceRow
	for _, r := range got {
		if r.Algo == "RXT" {
			rxt = r
		}
	}
	if rxt.Count != 0 {
		t.Errorf("RXT Count = %d, want 0", rxt.Count)
	}
	if rxt.AvgDifficultyDisplay != "0.00" {
		t.Errorf("RXT AvgDifficultyDisplay = %q, want %q", rxt.AvgDifficultyDisplay, "0.00")
	}
	if rxt.CurrentDifficultyDisplay != "0" {
		t.Errorf("RXT CurrentDifficultyDisplay = %q, want %q", rxt.CurrentDifficultyDisplay, "0")
	}
	if rxt.LastMinedDifficultyDisplay != "—" {
		t.Errorf("RXT LastMinedDifficultyDisplay = %q, want %q", rxt.LastMinedDifficultyDisplay, "—")
	}
	if rxt.LastMinedAdjustedDifficultyDisplay != "—" {
		t.Errorf("RXT LastMinedAdjustedDifficultyDisplay = %q, want %q", rxt.LastMinedAdjustedDifficultyDisplay, "—")
	}
}

func TestNewAlgoGlanceRows_AlgoPresentOnlyInAlgos_CountAndAvgFromAlgosCurrentDiffDefaults(t *testing.T) {
	algos := []db.AlgoCountRow{
		{Algo: "C29", Count: 7, AvgDifficulty: 55.5},
	}

	got := newAlgoGlanceRows(algos, nil, nil)
	wantAlgoOrder(t, got)

	var c29 algoGlanceRow
	for _, r := range got {
		if r.Algo == "C29" {
			c29 = r
		}
	}
	if c29.Count != 7 {
		t.Errorf("C29 Count = %d, want 7", c29.Count)
	}
	if c29.AvgDifficultyDisplay != "55.50" {
		t.Errorf("C29 AvgDifficultyDisplay = %q, want %q", c29.AvgDifficultyDisplay, "55.50")
	}
	if c29.CurrentDifficultyDisplay != "0" {
		t.Errorf("C29 CurrentDifficultyDisplay = %q, want %q", c29.CurrentDifficultyDisplay, "0")
	}
	if c29.LastMinedDifficultyDisplay != "—" {
		t.Errorf("C29 LastMinedDifficultyDisplay = %q, want %q", c29.LastMinedDifficultyDisplay, "—")
	}
	if c29.LastMinedAdjustedDifficultyDisplay != "—" {
		t.Errorf("C29 LastMinedAdjustedDifficultyDisplay = %q, want %q", c29.LastMinedAdjustedDifficultyDisplay, "—")
	}
}

func TestNewAlgoGlanceRows_AlgoPresentOnlyInSnapshots_CurrentDiffFromSnapshotsCountAndAvgDefault(t *testing.T) {
	snapshots := []db.TemplateDifficultySnapshot{
		{Algo: "SHA3X", TargetDifficulty: 4242, Height: 500},
	}

	got := newAlgoGlanceRows(nil, snapshots, nil)
	wantAlgoOrder(t, got)

	var sha3x algoGlanceRow
	for _, r := range got {
		if r.Algo == "SHA3X" {
			sha3x = r
		}
	}
	if sha3x.Count != 0 {
		t.Errorf("SHA3X Count = %d, want 0", sha3x.Count)
	}
	if sha3x.AvgDifficultyDisplay != "0.00" {
		t.Errorf("SHA3X AvgDifficultyDisplay = %q, want %q", sha3x.AvgDifficultyDisplay, "0.00")
	}
	if sha3x.CurrentDifficultyDisplay != "4,242" {
		t.Errorf("SHA3X CurrentDifficultyDisplay = %q, want %q", sha3x.CurrentDifficultyDisplay, "4,242")
	}
}

func TestNewAlgoGlanceRows_InputOrderDoesNotAffectOutputOrder(t *testing.T) {
	// Inputs deliberately out of analysis.AlgoOrder order.
	algos := []db.AlgoCountRow{
		{Algo: "SHA3X", Count: 4, AvgDifficulty: 4},
		{Algo: "RXM", Count: 1, AvgDifficulty: 1},
		{Algo: "C29", Count: 3, AvgDifficulty: 3},
		{Algo: "RXT", Count: 2, AvgDifficulty: 2},
	}
	snapshots := []db.TemplateDifficultySnapshot{
		{Algo: "C29", TargetDifficulty: 30},
		{Algo: "SHA3X", TargetDifficulty: 40},
		{Algo: "RXT", TargetDifficulty: 20},
		{Algo: "RXM", TargetDifficulty: 10},
	}
	lastMined := []db.DifficultySnapshot{
		{Algo: "RXT", Difficulty: 200},
		{Algo: "RXM", Difficulty: 100},
		{Algo: "SHA3X", Difficulty: 400},
		{Algo: "C29", Difficulty: 300},
	}

	got := newAlgoGlanceRows(algos, snapshots, lastMined)
	wantAlgoOrder(t, got)

	gotAlgos := make([]string, len(got))
	for i, r := range got {
		gotAlgos[i] = r.Algo
	}
	if !reflect.DeepEqual(gotAlgos, analysis.AlgoOrder) {
		t.Errorf("output algo order = %v, want %v", gotAlgos, analysis.AlgoOrder)
	}
}

// TestNewAlgoGlanceRows_LastMinedPresentWithAdjustedValue_UsesRealValues proves that
// when an algo has a difficulty_snapshots row with a non-NULL AdjustedDifficulty, both
// LastMinedDifficultyDisplay and LastMinedAdjustedDifficultyDisplay render the real
// (humanized) values rather than the "—" missing-data placeholder.
func TestNewAlgoGlanceRows_LastMinedPresentWithAdjustedValue_UsesRealValues(t *testing.T) {
	adjusted := int64(54321)
	lastMined := []db.DifficultySnapshot{
		{Algo: "RXM", Height: 1000, Difficulty: 12345, AdjustedDifficulty: &adjusted},
	}

	got := newAlgoGlanceRows(nil, nil, lastMined)
	wantAlgoOrder(t, got)

	row := got[0] // RXM is analysis.AlgoOrder[0]
	if row.LastMinedDifficultyDisplay != "12,345" {
		t.Errorf("LastMinedDifficultyDisplay = %q, want %q", row.LastMinedDifficultyDisplay, "12,345")
	}
	if row.LastMinedAdjustedDifficultyDisplay != "54,321" {
		t.Errorf("LastMinedAdjustedDifficultyDisplay = %q, want %q", row.LastMinedAdjustedDifficultyDisplay, "54,321")
	}
}

// TestNewAlgoGlanceRows_LastMinedPresentWithNilAdjusted_RawShownAdjustedPlaceholder
// proves that an algo whose difficulty_snapshots row HAS been captured (so
// LastMinedDifficultyDisplay shows the real raw value) but whose AdjustedDifficulty is
// still NULL (not yet backfilled/captured for that block) renders
// LastMinedAdjustedDifficultyDisplay as "—", not "0" - the core nullable-vs-zero
// distinction this feature depends on.
func TestNewAlgoGlanceRows_LastMinedPresentWithNilAdjusted_RawShownAdjustedPlaceholder(t *testing.T) {
	lastMined := []db.DifficultySnapshot{
		{Algo: "SHA3X", Height: 2000, Difficulty: 99999, AdjustedDifficulty: nil},
	}

	got := newAlgoGlanceRows(nil, nil, lastMined)
	wantAlgoOrder(t, got)

	var sha3x algoGlanceRow
	for _, r := range got {
		if r.Algo == "SHA3X" {
			sha3x = r
		}
	}
	if sha3x.LastMinedDifficultyDisplay != "99,999" {
		t.Errorf("LastMinedDifficultyDisplay = %q, want %q", sha3x.LastMinedDifficultyDisplay, "99,999")
	}
	if sha3x.LastMinedAdjustedDifficultyDisplay != "—" {
		t.Errorf("LastMinedAdjustedDifficultyDisplay = %q, want %q (NULL must not render as 0)", sha3x.LastMinedAdjustedDifficultyDisplay, "—")
	}
}

// TestNewAlgoGlanceRows_LastMinedAdjustedZeroIsDistinctFromNil proves a real captured
// 0 (AdjustedDifficulty pointing at int64(0), as opposed to a nil pointer) renders as
// the humanized "0", not the "—" missing-data placeholder - the two states must stay
// distinguishable all the way through display.
func TestNewAlgoGlanceRows_LastMinedAdjustedZeroIsDistinctFromNil(t *testing.T) {
	zero := int64(0)
	lastMined := []db.DifficultySnapshot{
		{Algo: "RXT", Height: 3000, Difficulty: 500, AdjustedDifficulty: &zero},
	}

	got := newAlgoGlanceRows(nil, nil, lastMined)
	wantAlgoOrder(t, got)

	var rxt algoGlanceRow
	for _, r := range got {
		if r.Algo == "RXT" {
			rxt = r
		}
	}
	if rxt.LastMinedAdjustedDifficultyDisplay != "0" {
		t.Errorf("LastMinedAdjustedDifficultyDisplay = %q, want %q (a real captured 0 must render as 0, not —)", rxt.LastMinedAdjustedDifficultyDisplay, "0")
	}
}
