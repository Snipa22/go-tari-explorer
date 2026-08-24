package chartrender

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func decodePNG(t *testing.T, data []byte) image.Config {
	t.Helper()
	if len(data) == 0 {
		t.Fatalf("expected non-empty PNG bytes")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("png.DecodeConfig: %v (not a valid PNG)", err)
	}
	// Also fully decode to make sure the pixel data itself is well-formed, not just the
	// header.
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("png.Decode: %v", err)
	}
	return cfg
}

func TestStackedAreaChart_ValidPNGWithExpectedDimensions(t *testing.T) {
	points := []Point{
		{X: 0, Series: map[string]float64{"RXM": 3, "RXT": 1, "C29": 0, "SHA3X": 2}},
		{X: 1000, Series: map[string]float64{"RXM": 5, "RXT": 0, "C29": 1, "SHA3X": 4}},
		{X: 2000, Series: map[string]float64{"RXM": 2, "RXT": 2, "C29": 2, "SHA3X": 2}},
	}
	seriesOrder := []string{"RXM", "RXT", "C29", "SHA3X"}

	data, err := StackedAreaChart(points, seriesOrder, "Algo Distribution", "height", "blocks")
	if err != nil {
		t.Fatalf("StackedAreaChart: %v", err)
	}

	cfg := decodePNG(t, data)
	if cfg.Width != DefaultWidth || cfg.Height != DefaultHeight {
		t.Errorf("dimensions = %dx%d, want %dx%d", cfg.Width, cfg.Height, DefaultWidth, DefaultHeight)
	}
}

func TestStackedAreaChart_NoSeries(t *testing.T) {
	points := []Point{{X: 0, Series: map[string]float64{"a": 1}}}
	if _, err := StackedAreaChart(points, nil, "t", "x", "y"); err == nil {
		t.Error("expected error for empty series order, got nil")
	}
}

func TestStackedAreaChart_UnsortedInputSortedByX(t *testing.T) {
	// Points intentionally out of X order - the function must sort internally rather
	// than assume caller ordering (DB results are already ordered, but this function
	// shouldn't silently produce a garbled chart if that assumption is ever violated).
	points := []Point{
		{X: 2000, Series: map[string]float64{"a": 1}},
		{X: 0, Series: map[string]float64{"a": 1}},
		{X: 1000, Series: map[string]float64{"a": 1}},
	}
	data, err := StackedAreaChart(points, []string{"a"}, "t", "x", "y")
	if err != nil {
		t.Fatalf("StackedAreaChart: %v", err)
	}
	decodePNG(t, data)
}

func TestLineChart_ValidPNGSingleSeries(t *testing.T) {
	points := []Point{
		{X: 0, Series: map[string]float64{"avg difficulty": 100}},
		{X: 1000, Series: map[string]float64{"avg difficulty": 150}},
		{X: 2000, Series: map[string]float64{"avg difficulty": 90}},
	}
	data, err := LineChart(points, []string{"avg difficulty"}, "Difficulty", "height", "difficulty")
	if err != nil {
		t.Fatalf("LineChart: %v", err)
	}
	cfg := decodePNG(t, data)
	if cfg.Width != DefaultWidth || cfg.Height != DefaultHeight {
		t.Errorf("dimensions = %dx%d, want %dx%d", cfg.Width, cfg.Height, DefaultWidth, DefaultHeight)
	}
}

func TestLineChart_MultiSeries(t *testing.T) {
	points := []Point{
		{X: 0, Series: map[string]float64{"a": 1, "b": 2}},
		{X: 1, Series: map[string]float64{"a": 3, "b": 1}},
	}
	data, err := LineChart(points, []string{"a", "b"}, "t", "x", "y")
	if err != nil {
		t.Fatalf("LineChart: %v", err)
	}
	decodePNG(t, data)
}

func TestLineChart_NoSeries(t *testing.T) {
	points := []Point{{X: 0, Series: map[string]float64{"a": 1}}}
	if _, err := LineChart(points, nil, "t", "x", "y"); err == nil {
		t.Error("expected error for empty series order, got nil")
	}
}

func TestSeriesValues_MissingKeyTreatedAsZero(t *testing.T) {
	points := []Point{
		{X: 0, Series: map[string]float64{"a": 1}},
		{X: 1, Series: map[string]float64{}}, // "a" absent -> should read as 0
	}
	got := seriesValues(points, "a")
	want := []float64{1, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("seriesValues[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// --- Fix 1 regression/coverage tests: <2 distinct X values must render a valid,
// decodable placeholder PNG with no error, instead of erroring (which is what the
// underlying go-chart library does/did - "zero x-range delta" - and which every
// /analysis/*.png handler previously turned into a raw HTTP 500 for any caller whose
// bucket_size/from/to query params collapsed the requested height range down this
// far). The >=2-distinct-X path (already covered by the tests above) must be
// unaffected by this change.

func TestStackedAreaChart_NoPoints_RendersPlaceholderNotError(t *testing.T) {
	data, err := StackedAreaChart(nil, []string{"a"}, "t", "x", "y")
	if err != nil {
		t.Fatalf("StackedAreaChart: expected no error for 0 points (placeholder path), got %v", err)
	}
	cfg := decodePNG(t, data)
	if cfg.Width != DefaultWidth || cfg.Height != DefaultHeight {
		t.Errorf("placeholder dimensions = %dx%d, want %dx%d", cfg.Width, cfg.Height, DefaultWidth, DefaultHeight)
	}
}

func TestStackedAreaChart_SinglePoint_RendersPlaceholderNotError(t *testing.T) {
	points := []Point{{X: 500, Series: map[string]float64{"a": 3}}}
	data, err := StackedAreaChart(points, []string{"a"}, "t", "x", "y")
	if err != nil {
		t.Fatalf("StackedAreaChart: expected no error for 1 point (placeholder path), got %v", err)
	}
	decodePNG(t, data)
}

func TestStackedAreaChart_AllSameX_RendersPlaceholderNotError(t *testing.T) {
	// Multiple points but all sharing one X value is the same "<2 distinct X"
	// shape go-chart can't range an axis for, even though len(points) > 1.
	points := []Point{
		{X: 500, Series: map[string]float64{"a": 1}},
		{X: 500, Series: map[string]float64{"a": 2}},
	}
	data, err := StackedAreaChart(points, []string{"a"}, "t", "x", "y")
	if err != nil {
		t.Fatalf("StackedAreaChart: expected no error for all-same-X points (placeholder path), got %v", err)
	}
	decodePNG(t, data)
}

func TestStackedAreaChart_NoPointsAndNoSeries_RendersPlaceholderNotError(t *testing.T) {
	// Distinct from TestStackedAreaChart_NoPoints_RendersPlaceholderNotError (which
	// passes a non-empty seriesOrder) and TestStackedAreaChart_NoSeries_StillErrors
	// (which passes non-empty points): when there is genuinely nothing at all - e.g.
	// internal/analysis.PoolShare's height range matched zero DB rows, so both points
	// and seriesOrder came back empty - this must render the placeholder, not error.
	data, err := StackedAreaChart(nil, nil, "t", "x", "y")
	if err != nil {
		t.Fatalf("StackedAreaChart: expected no error for 0 points and 0 series (placeholder path), got %v", err)
	}
	decodePNG(t, data)
}

func TestStackedAreaChart_NoSeries_StillErrors(t *testing.T) {
	// Unrelated to the <2-distinct-X fix: no series names is still a caller
	// programming error, not a "not enough data" shape, and must still error.
	points := []Point{{X: 0, Series: map[string]float64{"a": 1}}}
	if _, err := StackedAreaChart(points, nil, "t", "x", "y"); err == nil {
		t.Error("expected error for empty series order, got nil")
	}
}

func TestLineChart_NoPoints_RendersPlaceholderNotError(t *testing.T) {
	data, err := LineChart(nil, []string{"a"}, "t", "x", "y")
	if err != nil {
		t.Fatalf("LineChart: expected no error for 0 points (placeholder path), got %v", err)
	}
	decodePNG(t, data)
}

func TestLineChart_SinglePoint_RendersPlaceholderNotError(t *testing.T) {
	points := []Point{{X: 500, Series: map[string]float64{"avg difficulty": 42}}}
	data, err := LineChart(points, []string{"avg difficulty"}, "Difficulty", "height", "difficulty")
	if err != nil {
		t.Fatalf("LineChart: expected no error for 1 point (placeholder path), got %v", err)
	}
	decodePNG(t, data)
}

func TestLineChart_NoPointsAndNoSeries_RendersPlaceholderNotError(t *testing.T) {
	// Distinct from TestLineChart_NoPoints_RendersPlaceholderNotError (which passes a
	// non-empty seriesOrder) and TestLineChart_NoSeries_StillErrors (which passes
	// non-empty points): when there is genuinely nothing at all - e.g.
	// internal/analysis.PoolShare's height range matched zero DB rows, so both points
	// and seriesOrder came back empty - this must render the placeholder, not error.
	data, err := LineChart(nil, nil, "t", "x", "y")
	if err != nil {
		t.Fatalf("LineChart: expected no error for 0 points and 0 series (placeholder path), got %v", err)
	}
	decodePNG(t, data)
}

func TestLineChart_NoSeries_StillErrors(t *testing.T) {
	points := []Point{{X: 0, Series: map[string]float64{"a": 1}}}
	if _, err := LineChart(points, nil, "t", "x", "y"); err == nil {
		t.Error("expected error for empty series order, got nil")
	}
}

func TestDistinctXCount(t *testing.T) {
	cases := []struct {
		name   string
		points []Point
		want   int
	}{
		{"nil", nil, 0},
		{"one", []Point{{X: 1}}, 1},
		{"two distinct", []Point{{X: 1}, {X: 2}}, 2},
		{"two same", []Point{{X: 1}, {X: 1}}, 1},
		{"three, two distinct", []Point{{X: 1}, {X: 2}, {X: 1}}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := distinctXCount(tc.points); got != tc.want {
				t.Errorf("distinctXCount(%v) = %d, want %d", tc.points, got, tc.want)
			}
		})
	}
}
