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

func TestStackedAreaChart_NoPoints(t *testing.T) {
	if _, err := StackedAreaChart(nil, []string{"a"}, "t", "x", "y"); err == nil {
		t.Error("expected error for empty points, got nil")
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

func TestLineChart_NoPoints(t *testing.T) {
	if _, err := LineChart(nil, []string{"a"}, "t", "x", "y"); err == nil {
		t.Error("expected error for empty points, got nil")
	}
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
