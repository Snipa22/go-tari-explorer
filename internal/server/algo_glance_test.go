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
	got := newAlgoGlanceRows(nil, nil)
	wantAlgoOrder(t, got)
}

func TestNewAlgoGlanceRows_AlgoPresentInBothInputs_UsesRealValues(t *testing.T) {
	algos := []db.AlgoCountRow{
		{Algo: "RXM", Count: 42, AvgDifficulty: 1234.5},
	}
	snapshots := []db.DifficultySnapshot{
		{Algo: "RXM", Difficulty: 9876, Height: 100},
	}

	got := newAlgoGlanceRows(algos, snapshots)
	wantAlgoOrder(t, got)

	row := got[0] // RXM is analysis.AlgoOrder[0]
	if row.Count != 42 {
		t.Errorf("Count = %d, want 42", row.Count)
	}
	if row.AvgDifficultyDisplay != "1234.50" {
		t.Errorf("AvgDifficultyDisplay = %q, want %q", row.AvgDifficultyDisplay, "1234.50")
	}
	if row.CurrentDifficultyDisplay != "9876" {
		t.Errorf("CurrentDifficultyDisplay = %q, want %q", row.CurrentDifficultyDisplay, "9876")
	}
}

func TestNewAlgoGlanceRows_AlgoPresentInNeitherInput_DefaultsGracefully(t *testing.T) {
	// RXT present in neither input.
	algos := []db.AlgoCountRow{
		{Algo: "RXM", Count: 1, AvgDifficulty: 1},
	}
	snapshots := []db.DifficultySnapshot{
		{Algo: "RXM", Difficulty: 1},
	}

	got := newAlgoGlanceRows(algos, snapshots)
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
}

func TestNewAlgoGlanceRows_AlgoPresentOnlyInAlgos_CountAndAvgFromAlgosCurrentDiffDefaults(t *testing.T) {
	algos := []db.AlgoCountRow{
		{Algo: "C29", Count: 7, AvgDifficulty: 55.5},
	}

	got := newAlgoGlanceRows(algos, nil)
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
}

func TestNewAlgoGlanceRows_AlgoPresentOnlyInSnapshots_CurrentDiffFromSnapshotsCountAndAvgDefault(t *testing.T) {
	snapshots := []db.DifficultySnapshot{
		{Algo: "SHA3X", Difficulty: 4242, Height: 500},
	}

	got := newAlgoGlanceRows(nil, snapshots)
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
	if sha3x.CurrentDifficultyDisplay != "4242" {
		t.Errorf("SHA3X CurrentDifficultyDisplay = %q, want %q", sha3x.CurrentDifficultyDisplay, "4242")
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
	snapshots := []db.DifficultySnapshot{
		{Algo: "C29", Difficulty: 30},
		{Algo: "SHA3X", Difficulty: 40},
		{Algo: "RXT", Difficulty: 20},
		{Algo: "RXM", Difficulty: 10},
	}

	got := newAlgoGlanceRows(algos, snapshots)
	wantAlgoOrder(t, got)

	gotAlgos := make([]string, len(got))
	for i, r := range got {
		gotAlgos[i] = r.Algo
	}
	if !reflect.DeepEqual(gotAlgos, analysis.AlgoOrder) {
		t.Errorf("output algo order = %v, want %v", gotAlgos, analysis.AlgoOrder)
	}
}
