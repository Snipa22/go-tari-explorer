package server

import (
	"bytes"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Snipa22/go-tari-explorer/internal/analysis"
	"github.com/Snipa22/go-tari-explorer/internal/chartrender"
)

// TestParseDifficultyAlgo covers parseDifficultyAlgo's graceful-degrade defaulting:
// missing, blank, and unknown ?algo= values all fall back to analysis.AlgoOrder[0],
// while any exact analysis.AlgoOrder member passes through unchanged.
func TestParseDifficultyAlgo(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"missing", "/analysis/difficulty.png", analysis.AlgoOrder[0]},
		{"blank", "/analysis/difficulty.png?algo=", analysis.AlgoOrder[0]},
		{"unknown", "/analysis/difficulty.png?algo=NOT_A_REAL_ALGO", analysis.AlgoOrder[0]},
		{"RXM", "/analysis/difficulty.png?algo=RXM", "RXM"},
		{"SHA3X", "/analysis/difficulty.png?algo=SHA3X", "SHA3X"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.url, nil)
			if got := parseDifficultyAlgo(r); got != tc.want {
				t.Errorf("parseDifficultyAlgo(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

// TestAllBucketsEmptyForAlgo covers the "genuinely zero data across the whole range"
// detection used by renderDifficultyAlgoPNG, distinguishing it from a single missing
// bucket (which must NOT be treated as all-empty).
func TestAllBucketsEmptyForAlgo(t *testing.T) {
	cases := []struct {
		name   string
		points []chartrender.Point
		want   bool
	}{
		{"no points at all", nil, true},
		{"every bucket empty", []chartrender.Point{
			{X: 0, Series: map[string]float64{}},
			{X: 1000, Series: map[string]float64{}},
		}, true},
		{"one bucket has data", []chartrender.Point{
			{X: 0, Series: map[string]float64{}},
			{X: 1000, Series: map[string]float64{"RXM": 5}},
		}, false},
		{"all buckets have data", []chartrender.Point{
			{X: 0, Series: map[string]float64{"RXM": 5}},
			{X: 1000, Series: map[string]float64{"RXM": 6}},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := allBucketsEmptyForAlgo(tc.points); got != tc.want {
				t.Errorf("allBucketsEmptyForAlgo(%+v) = %v, want %v", tc.points, got, tc.want)
			}
		})
	}
}

// TestRenderDifficultyAlgoPNG_ZeroDataRendersPlaceholder proves that an algo with
// genuinely zero data across the entire requested range renders chartrender's
// "not enough data" placeholder byte-for-byte (matching a direct
// chartrender.NotEnoughDataPNG call with the same title), while a real-data algo
// present in the very same points list renders a real, larger, structurally
// different PNG - not a degenerate flat-zero-line chart and not an HTTP error.
func TestRenderDifficultyAlgoPNG_ZeroDataRendersPlaceholder(t *testing.T) {
	// C29 is entirely absent from every bucket's Series map (zero blocks across the
	// whole range); RXM has real, varying data across the same buckets.
	points := []chartrender.Point{
		{X: 0, Series: map[string]float64{"RXM": 100}},
		{X: 1000, Series: map[string]float64{"RXM": 250}},
		{X: 2000, Series: map[string]float64{"RXM": 400}},
	}

	const title = "Difficulty (C29, avg, hashrate proxy)"
	gotPlaceholder, err := renderDifficultyAlgoPNG(points, "C29", title)
	if err != nil {
		t.Fatalf("renderDifficultyAlgoPNG(C29): %v", err)
	}
	wantPlaceholder, err := chartrender.NotEnoughDataPNG(title)
	if err != nil {
		t.Fatalf("chartrender.NotEnoughDataPNG: %v", err)
	}
	if !bytes.Equal(gotPlaceholder, wantPlaceholder) {
		t.Error("renderDifficultyAlgoPNG for a whole-range-zero-data algo did not match chartrender.NotEnoughDataPNG byte-for-byte")
	}

	rxmTitle := "Difficulty (RXM, avg, hashrate proxy)"
	gotRXM, err := renderDifficultyAlgoPNG(points, "RXM", rxmTitle)
	if err != nil {
		t.Fatalf("renderDifficultyAlgoPNG(RXM): %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(gotRXM)); err != nil {
		t.Errorf("RXM PNG failed to decode: %v", err)
	}
	if bytes.Equal(gotRXM, gotPlaceholder) {
		t.Error("real-data RXM chart must not be byte-identical to the not-enough-data placeholder")
	}
	if len(gotRXM) <= len(gotPlaceholder) {
		t.Errorf("real-data RXM chart (%d bytes) expected to be larger than the plain placeholder (%d bytes)", len(gotRXM), len(gotPlaceholder))
	}
}
