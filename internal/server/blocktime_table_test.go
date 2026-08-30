package server

import (
	"testing"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// float64Ptr/int64Ptr are small test-local helpers for building
// db.BlockTimeBucketRow's pointer fields inline.
func float64Ptr(v float64) *float64 { return &v }
func int64Ptr(v int64) *int64       { return &v }

// TestNewBlockTimeBucketTableView_AllStatsPresent proves a normal row with all stats
// present renders correctly-formatted strings: 2-decimal seconds for
// mean/median/stddev, and plain-integer strings for the bucket height, max, and sample
// count. This test needs no live DB - it's pure struct-in/string-out - so it must run
// every time, not skip.
func TestNewBlockTimeBucketTableView_AllStatsPresent(t *testing.T) {
	rows := []db.BlockTimeBucketRow{
		{
			BucketStart:   5000,
			BucketEnd:     5999,
			MeanSeconds:   float64Ptr(20.5),
			MedianSeconds: float64Ptr(18.25),
			StdDevSeconds: float64Ptr(4.126),
			MaxSeconds:    int64Ptr(60),
			SampleCount:   42,
		},
	}

	view := newBlockTimeBucketTableView(rows)

	wantCols := []string{"Bucket", "Mean (s)", "Median (s)", "StdDev (s)", "Max (s)", "Samples"}
	if len(view.Columns) != len(wantCols) {
		t.Fatalf("Columns = %v, want %v", view.Columns, wantCols)
	}
	for i, c := range wantCols {
		if view.Columns[i] != c {
			t.Errorf("Columns[%d] = %q, want %q", i, view.Columns[i], c)
		}
	}

	if len(view.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1", len(view.Rows))
	}
	row := view.Rows[0]
	if row.Height != "5,000" {
		t.Errorf("Height = %q, want %q", row.Height, "5,000")
	}
	wantValues := []string{"20.50", "18.25", "4.13", "60", "42"}
	if len(row.Values) != len(wantValues) {
		t.Fatalf("Values = %v, want %v", row.Values, wantValues)
	}
	for i, want := range wantValues {
		if row.Values[i] != want {
			t.Errorf("Values[%d] = %q, want %q", i, row.Values[i], want)
		}
	}
}

// TestNewBlockTimeBucketTableView_NilStats proves a row with
// Mean/Median/StdDev/MaxSeconds all nil (a zero-sample bucket) renders "n/a" for every
// one of those 4 cells - never a panic, never "0.00" - while SampleCount 0 still
// renders as the plain integer "0".
func TestNewBlockTimeBucketTableView_NilStats(t *testing.T) {
	rows := []db.BlockTimeBucketRow{
		{
			BucketStart:   9000,
			BucketEnd:     9999,
			MeanSeconds:   nil,
			MedianSeconds: nil,
			StdDevSeconds: nil,
			MaxSeconds:    nil,
			SampleCount:   0,
		},
	}

	view := newBlockTimeBucketTableView(rows)

	if len(view.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1", len(view.Rows))
	}
	row := view.Rows[0]
	if row.Height != "9,000" {
		t.Errorf("Height = %q, want %q", row.Height, "9,000")
	}
	wantValues := []string{"n/a", "n/a", "n/a", "n/a", "0"}
	if len(row.Values) != len(wantValues) {
		t.Fatalf("Values = %v, want %v", row.Values, wantValues)
	}
	for i, want := range wantValues {
		if row.Values[i] != want {
			t.Errorf("Values[%d] = %q, want %q", i, row.Values[i], want)
		}
	}
}

// TestNewBlockTimeBucketTableView_EmptyRows proves an empty rows slice never panics
// and produces a sane header-only result (zero data rows), matching
// newAnalysisTableView's own empty-input behavior.
func TestNewBlockTimeBucketTableView_EmptyRows(t *testing.T) {
	view := newBlockTimeBucketTableView(nil)

	if view == nil {
		t.Fatal("newBlockTimeBucketTableView returned nil for empty rows")
	}
	if len(view.Rows) != 0 {
		t.Errorf("len(Rows) = %d, want 0", len(view.Rows))
	}
	wantCols := []string{"Bucket", "Mean (s)", "Median (s)", "StdDev (s)", "Max (s)", "Samples"}
	if len(view.Columns) != len(wantCols) {
		t.Fatalf("Columns = %v, want %v", view.Columns, wantCols)
	}
}
